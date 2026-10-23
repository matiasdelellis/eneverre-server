// Package zaguan publishes the Eneverre camera inventory to a Zaguán hub.
//
// A Zaguán hub owns the devices of a home. Providers (this server, a Zigbee
// bridge, a Tuya account, ...) speak its Source API: they publish a snapshot
// of their devices, keep a lease alive and receive commands through a long
// poll. The client of that contract lives in
// github.com/getzaguan/zaguan-sourceapi, so this package only maps the
// cameras and runs the loops.
//
// One source carries every camera: each camera becomes a device with a
// single `camera` entity. The source is disabled unless [zaguan] url and a
// token are configured; see doc/ZAGUAN.md.
package zaguan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getzaguan/zaguan-sourceapi"

	"eneverre/internal/camera"
)

// Defaults of the publisher. The Source API requires a heartbeat at least
// every 90 seconds; the inventory is republished on every sync so a camera
// that connects or disconnects shows up without waiting for a resync.
const (
	DefaultSyncInterval      = 30 * time.Second
	DefaultHeartbeatInterval = 30 * time.Second
	DefaultCommandWait       = 25 * time.Second
	snapshotRetry            = 10 * time.Second
	commandRetry             = 5 * time.Second
)

// Identity of the source in the hub. The prefixes are stable: the hub keys
// devices and entities by them, so changing one would duplicate everything.
const (
	uniquePrefix = "eneverre:"
	entitySuffix = "/camera"
	manufacturer = "Eneverre"
	model        = "Camera"
	entityName   = "Camera"
	// cameraDomain is catalog.DomainCamera. The literal keeps the mapping in
	// one place; the hub refuses a domain it does not know.
	cameraDomain = "camera"
)

// Camera is one camera as the publisher needs it. It is deliberately smaller
// than camera.Camera: the server builds it with the live privacy state, the
// engine status and the public URLs, so this package never reads the DB or
// the media engine.
type Camera struct {
	ID       string
	Name     string
	Comment  string
	Location string

	Enabled     bool
	Privacy     bool
	ScheduleOff bool
	Connected   bool
	// Live is whether the camera has a live feed configured (MSE or relay);
	// it does not flap with connectivity or privacy.
	Live bool
	// Recording is whether segments are being written right now; Record is
	// whether recording is enabled for the camera at all.
	Recording bool
	Record    bool

	Width  int
	Height int

	// URLs are absolute when the operator configured [zaguan] public_url
	// (RTSP may also come from [media] rtsp_host). Empty means the attribute
	// is not published. The HTTP media URLs carry the rotating relay
	// credentials as query parameters, so the hub's web client can play them
	// without an Eneverre session.
	RTSP        string
	MSEURL      string
	MSEInfoURL  string
	HLSURL      string
	SnapshotURL string

	Snapshot bool // the camera can produce a still JPEG
	Playback bool
	Talk     bool
	PTZ      bool
	// PTZHome is true when the camera has a configured absolute home
	// position. The `ptz_home` command is only advertised then; without it the
	// firmware would be told to move to the -1 sentinel coordinates.
	PTZHome bool
}

// Config configures a Publisher.
type Config struct {
	// BaseURL is the address of the Zaguán hub, with or without a scheme.
	BaseURL string
	// Token is the Source API token created in the hub (zsrc_...).
	Token string
	// Name is the source name shown in the hub's Sources page.
	Name string
	// Version is this server's version, published as the source metadata.
	Version string
	// Logger defaults to slog.Default().
	Logger *slog.Logger

	SyncInterval      time.Duration
	HeartbeatInterval time.Duration
	CommandWait       time.Duration

	// Cameras returns the current inventory. It is called for every snapshot
	// and for every command, so it must be safe for concurrent use.
	Cameras func() []Camera
	// Snapshot takes a fresh still from one camera. It is optional: when nil
	// (or when the camera has no snapshot endpoint) the `snapshot` command
	// answers with an error instead of pretending it worked.
	Snapshot func(ctx context.Context, cameraID string) error
	// PTZMove issues a relative pan/tilt move in degrees. The three PTZ
	// callbacks are optional and are only advertised for cameras with the
	// ptz capability.
	PTZMove        func(ctx context.Context, cameraID string, pan, tilt float64) error
	PTZHome        func(ctx context.Context, cameraID string) error
	PTZRecalibrate func(ctx context.Context, cameraID string) error
}

// Publisher keeps a Zaguán hub up to date with the cameras of this server.
type Publisher struct {
	cfg Config
	hub *sourceapi.Client
	log *slog.Logger

	mu        sync.Mutex
	version   uint64
	published bool
}

// New validates the configuration and returns a publisher. The caller still
// has to run it.
func New(cfg Config) (*Publisher, error) {
	if cfg.Cameras == nil {
		return nil, errors.New("zaguan: the camera source is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = "Eneverre"
	}
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = DefaultSyncInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.CommandWait <= 0 {
		cfg.CommandWait = DefaultCommandWait
	}
	hub, err := sourceapi.New(sourceapi.Config{BaseURL: cfg.BaseURL, Token: cfg.Token})
	if err != nil {
		return nil, err
	}
	return &Publisher{cfg: cfg, hub: hub, log: cfg.Logger}, nil
}

// Run publishes the source until ctx ends. The first snapshot is immediate;
// a failed hub does not stop the loops, it retries.
func (p *Publisher) Run(ctx context.Context) error {
	p.publishMeta(ctx)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.heartbeatLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		p.commandLoop(ctx)
	}()
	p.snapshotLoop(ctx)
	wg.Wait()
	return nil
}

// publishMeta updates the source name/kind/version in the hub. A failure is
// not fatal: the snapshot and the lease are what make the source usable.
func (p *Publisher) publishMeta(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := p.hub.Meta(callCtx, sourceapi.Meta{
		Name:    p.cfg.Name,
		Kind:    "eneverre",
		Version: p.cfg.Version,
	})
	if err != nil && ctx.Err() == nil {
		p.log.Warn("cannot publish the source metadata", "err", err)
	}
}

// snapshotLoop publishes the inventory every SyncInterval. There are no state
// events in this integration: the snapshot is small, so republishing it is
// the simplest way to keep connectivity and availability current, and it also
// restores the devices after a hub restart.
func (p *Publisher) snapshotLoop(ctx context.Context) {
	for {
		if p.publishSnapshot(ctx) {
			break
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(snapshotRetry):
		}
	}

	ticker := time.NewTicker(p.cfg.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.publishSnapshot(ctx)
		}
	}
}

// publishSnapshot sends the current inventory. It tells whether the hub took
// it. A version conflict means the hub has a newer snapshot (a restart with a
// turned-back clock, or another client): the next version above it is tried.
func (p *Publisher) publishSnapshot(ctx context.Context) bool {
	snap := p.buildSnapshot()
	p.nextVersion(&snap)

	for attempt := 0; attempt < 3; attempt++ {
		err := p.hub.Snapshot(ctx, snap)
		if err == nil {
			p.mu.Lock()
			first := !p.published
			p.published = true
			p.mu.Unlock()
			if first {
				p.log.Info("camera inventory published",
					"devices", len(snap.Devices), "entities", len(snap.Entities), "version", snap.Version)
			} else {
				p.log.Debug("camera inventory refreshed",
					"devices", len(snap.Devices), "entities", len(snap.Entities), "version", snap.Version)
			}
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if errors.Is(err, sourceapi.ErrUnauthorized) {
			p.log.Error("the hub rejected the source token; check [zaguan] token/token_file", "err", err)
			return false
		}
		var apiErr *sourceapi.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
			p.log.Warn("cannot publish the camera inventory", "err", err)
			return false
		}
		p.log.Info("the hub has a newer snapshot; retrying with the next version")
		p.nextVersion(&snap)
	}
	p.log.Warn("the camera inventory was refused three times")
	return false
}

// nextVersion stamps a snapshot with a version above the last one. The clock
// keeps versions increasing across restarts; the explicit bump covers a clock
// that went backwards and the 409 retry.
func (p *Publisher) nextVersion(snap *sourceapi.Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	version := uint64(time.Now().UnixMilli())
	if version <= p.version {
		version = p.version + 1
	}
	p.version = version
	snap.Version = version
}

// buildSnapshot maps the cameras to the hub model.
func (p *Publisher) buildSnapshot() sourceapi.Snapshot {
	cams := append([]Camera(nil), p.cfg.Cameras()...)
	sort.SliceStable(cams, func(i, j int) bool {
		if cams[i].Name != cams[j].Name {
			return cams[i].Name < cams[j].Name
		}
		return cams[i].ID < cams[j].ID
	})

	areas := map[string]string{}
	snap := sourceapi.Snapshot{}
	for _, c := range cams {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = c.ID
		}
		areaID := ""
		if location := strings.TrimSpace(c.Location); location != "" {
			areaID = areaUniqueID(location)
			areas[areaID] = location
		}
		available := cameraAvailable(c)

		snap.Devices = append(snap.Devices, sourceapi.Device{
			UniqueID:     deviceUniqueID(c.ID),
			Name:         name,
			Manufacturer: manufacturer,
			Model:        model,
			SWVersion:    p.cfg.Version,
			AreaID:       areaID,
			Disabled:     !c.Enabled,
		})
		// The Source API reads an omitted command list as "the catalog
		// defaults" and an explicit empty list as read-only. A camera
		// without a snapshot endpoint must not offer the button: it would
		// only answer with an error.
		commands := []string{}
		if c.Snapshot {
			commands = append(commands, "snapshot")
		}
		if c.PTZ {
			commands = append(commands, "ptz_move")
			if c.PTZHome {
				commands = append(commands, "ptz_home")
			}
			commands = append(commands, "ptz_recalibrate")
		}
		snap.Entities = append(snap.Entities, sourceapi.Entity{
			UniqueID:       entityUniqueID(c.ID),
			DeviceUniqueID: deviceUniqueID(c.ID),
			Domain:         cameraDomain,
			Name:           entityName,
			State:          stateOf(c),
			Attributes:     attributesOf(c),
			Capabilities:   capabilitiesOf(c),
			Commands:       commands,
			Available:      &available,
		})
	}

	for id, name := range areas {
		snap.Areas = append(snap.Areas, sourceapi.Area{ID: id, Name: name})
	}
	sort.Slice(snap.Areas, func(i, j int) bool { return snap.Areas[i].ID < snap.Areas[j].ID })
	return snap
}

// cameraAvailable reports whether the camera can be used right now. A paused
// pipeline (privacy or outside its recording schedule) and an unreachable
// camera are not available; the entity keeps its last state but the card is
// dimmed.
func cameraAvailable(c Camera) bool {
	return c.Enabled && c.Connected && !c.Privacy && !c.ScheduleOff
}

// stateOf is the camera domain state: streaming when the NVR has the camera,
// idle otherwise.
func stateOf(c Camera) string {
	if cameraAvailable(c) {
		return "streaming"
	}
	return "idle"
}

// capabilitiesOf advertises only what the camera can actually serve. The hub
// uses the list to grey out what the camera does not have.
func capabilitiesOf(c Camera) []string {
	caps := make([]string, 0, 4)
	if c.Live {
		caps = append(caps, "live")
	}
	if c.Snapshot {
		caps = append(caps, "snapshot")
	}
	if c.Record {
		caps = append(caps, "recording")
	}
	if c.PTZ {
		caps = append(caps, "ptz")
	}
	return caps
}

// attributesOf carries what the camera card needs beyond the state. Empty
// values are omitted so the hub store stays small.
func attributesOf(c Camera) map[string]any {
	attrs := map[string]any{
		"enabled":      c.Enabled,
		"privacy":      c.Privacy,
		"schedule_off": c.ScheduleOff,
		"recording":    c.Recording,
	}
	if c.Comment != "" {
		attrs["comment"] = c.Comment
	}
	if c.Width > 0 && c.Height > 0 {
		// Eneverre stores the sensor aspect ratio (16/9 by default), not the
		// encoded pixels: publish it as the ratio it is.
		attrs["aspect_ratio"] = fmt.Sprintf("%d:%d", c.Width, c.Height)
	}
	if c.RTSP != "" {
		attrs["rtsp_url"] = c.RTSP
	}
	if c.MSEURL != "" {
		attrs["mse_url"] = c.MSEURL
	}
	if c.MSEInfoURL != "" {
		attrs["mse_info_url"] = c.MSEInfoURL
	}
	if c.HLSURL != "" {
		attrs["hls_url"] = c.HLSURL
	}
	if c.SnapshotURL != "" {
		attrs["snapshot_url"] = c.SnapshotURL
	}
	if c.Playback {
		attrs["playback"] = true
	}
	if c.Talk {
		attrs["talk"] = true
	}
	if c.PTZ {
		attrs["ptz"] = true
	}
	return attrs
}

// heartbeatLoop keeps the lease of the source alive.
func (p *Publisher) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.hub.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				p.log.Warn("the heartbeat failed", "err", err)
			}
		}
	}
}

// commandLoop long-polls the hub for commands.
func (p *Publisher) commandLoop(ctx context.Context) {
	for {
		commands, err := p.hub.Commands(ctx, p.cfg.CommandWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Warn("the long poll of commands failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(commandRetry):
			}
			continue
		}
		for _, command := range commands {
			p.execute(ctx, command)
		}
	}
}

// execute runs one command and answers it. The camera domain offers snapshot
// and PTZ; the stream itself runs continuously.
func (p *Publisher) execute(ctx context.Context, command sourceapi.Command) {
	cam, ok := p.cameraByEntity(command.EntityUniqueID)
	if !ok {
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "unknown camera"})
		return
	}
	switch command.Command {
	case "snapshot":
		p.executeSnapshot(ctx, command, cam)
	case "ptz_move":
		p.executePTZMove(ctx, command, cam)
	case "ptz_home":
		p.executePTZ(ctx, command, cam, "the camera could not go home", p.cfg.PTZHome)
	case "ptz_recalibrate":
		p.executePTZ(ctx, command, cam, "the camera could not recalibrate", p.cfg.PTZRecalibrate)
	default:
		p.answer(ctx, command, sourceapi.CommandResult{
			OK:    false,
			Error: fmt.Sprintf("unsupported command %q", command.Command),
		})
	}
}

func (p *Publisher) executeSnapshot(ctx context.Context, command sourceapi.Command, cam *Camera) {
	switch {
	case !cam.Enabled:
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera is disabled"})
		return
	case cam.Privacy:
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera is in privacy mode"})
		return
	case cam.ScheduleOff:
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera is outside its recording schedule"})
		return
	case !cam.Snapshot:
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera has no snapshot endpoint"})
		return
	case p.cfg.Snapshot == nil:
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "snapshots are not configured"})
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := p.cfg.Snapshot(callCtx, cam.ID)
	cancel()
	if err != nil {
		// The detailed error stays in this log: upstream URLs can carry
		// camera credentials, and the hub stores and shows command errors.
		p.log.Warn("the camera snapshot failed", "camera", cam.ID, "err", err)
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera did not return a snapshot"})
		return
	}
	p.answerCamera(ctx, command, cam)
}

func (p *Publisher) executePTZMove(ctx context.Context, command sourceapi.Command, cam *Camera) {
	pan, okPan := numberArg(command.Args, "pan")
	tilt, okTilt := numberArg(command.Args, "tilt")
	if !okPan || !okTilt {
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "pan and tilt are required"})
		return
	}
	if !cam.Enabled {
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera is disabled"})
		return
	}
	if !cam.PTZ {
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera has no PTZ"})
		return
	}
	if p.cfg.PTZMove == nil {
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "PTZ is not configured"})
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := p.cfg.PTZMove(callCtx, cam.ID, pan, tilt)
	cancel()
	if err != nil {
		p.log.Warn("the camera PTZ move failed", "camera", cam.ID, "err", err)
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera could not move"})
		return
	}
	p.answerCamera(ctx, command, cam)
}

func (p *Publisher) executePTZ(ctx context.Context, command sourceapi.Command, cam *Camera, failure string, action func(context.Context, string) error) {
	if !cam.Enabled {
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera is disabled"})
		return
	}
	if !cam.PTZ {
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "the camera has no PTZ"})
		return
	}
	if action == nil {
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: "PTZ is not configured"})
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := action(callCtx, cam.ID)
	cancel()
	if err != nil {
		p.log.Warn("the camera PTZ command failed", "camera", cam.ID, "command", command.Command, "err", err)
		p.answer(ctx, command, sourceapi.CommandResult{OK: false, Error: failure})
		return
	}
	p.answerCamera(ctx, command, cam)
}

func (p *Publisher) answerCamera(ctx context.Context, command sourceapi.Command, cam *Camera) {
	p.answer(ctx, command, sourceapi.CommandResult{
		OK:         true,
		State:      stateOf(*cam),
		Attributes: attributesOf(*cam),
	})
}

func numberArg(args map[string]any, name string) (float64, bool) {
	switch value := args[name].(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	}
	return 0, false
}

// cameraByEntity finds the camera behind an entity unique id.
func (p *Publisher) cameraByEntity(uniqueID string) (*Camera, bool) {
	for _, c := range p.cfg.Cameras() {
		if entityUniqueID(c.ID) == uniqueID {
			cam := c
			return &cam, true
		}
	}
	return nil, false
}

// answer reports the result of a command. It has its own timeout because the
// long poll context lives until shutdown.
func (p *Publisher) answer(ctx context.Context, command sourceapi.Command, result sourceapi.CommandResult) {
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.hub.Result(callCtx, command.ID, result); err != nil && ctx.Err() == nil {
		p.log.Warn("cannot answer the command", "command", command.ID, "err", err)
	}
}

func deviceUniqueID(cameraID string) string { return uniquePrefix + cameraID }

func entityUniqueID(cameraID string) string { return deviceUniqueID(cameraID) + entitySuffix }

// areaUniqueID names a room by its location. The hub resolves the id or the
// name into its own rooms; the prefix keeps a location from colliding with a
// room another source proposed.
func areaUniqueID(location string) string {
	slug := camera.Slugify(location)
	if slug == "" {
		slug = "location"
	}
	return uniquePrefix + slug
}
