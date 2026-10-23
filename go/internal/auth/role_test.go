package auth

import "testing"

func TestRoleCapabilities(t *testing.T) {
	admin := CurrentUser{Role: RoleAdmin}
	for _, cap := range []Capability{
		CapStatusRead, CapCamerasRead, CapEventsRead, CapRecordingsRead,
		CapMetricsRead, CapLive, CapPTZ, CapPrivacy, CapTalk, CapSettings, CapAdmin,
	} {
		if !admin.Can(cap) {
			t.Errorf("admin lacks %s", cap)
		}
	}

	viewer := CurrentUser{Role: RoleViewer}
	for _, cap := range []Capability{CapCamerasRead, CapEventsRead, CapLive} {
		if !viewer.Can(cap) {
			t.Errorf("viewer lacks %s", cap)
		}
	}
	if viewer.Can(CapRecordingsRead) {
		t.Error("viewer must not have playback/recordings")
	}
	for _, cap := range []Capability{CapPTZ, CapPrivacy, CapTalk, CapAdmin} {
		if viewer.Can(cap) {
			t.Errorf("viewer must not have %s", cap)
		}
	}

	monitor := CurrentUser{Role: RoleMonitor}
	if !monitor.Can(CapStatusRead) || !monitor.Can(CapMetricsRead) {
		t.Error("monitor must have status and metrics")
	}
	for _, cap := range []Capability{CapCamerasRead, CapEventsRead, CapRecordingsRead, CapLive, CapPTZ, CapPrivacy, CapTalk, CapAdmin} {
		if monitor.Can(cap) {
			t.Errorf("monitor must not have %s", cap)
		}
	}

	user := CurrentUser{Role: RoleUser}
	for _, cap := range []Capability{CapCamerasRead, CapEventsRead, CapRecordingsRead, CapLive, CapPTZ, CapPrivacy, CapTalk} {
		if !user.Can(cap) {
			t.Errorf("user lacks %s", cap)
		}
	}
	if user.Can(CapAdmin) || user.Can(CapStatusRead) || user.Can(CapMetricsRead) {
		t.Error("user must not have admin/status/metrics")
	}
}

func TestValidRole(t *testing.T) {
	for _, role := range []string{RoleAdmin, RoleUser, RoleViewer, RoleMonitor} {
		if !ValidRole(role) {
			t.Errorf("ValidRole(%q) = false", role)
		}
	}
	for _, role := range []string{"", "superuser", "Admin"} {
		if ValidRole(role) {
			t.Errorf("ValidRole(%q) = true", role)
		}
	}
}
