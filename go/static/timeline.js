// Timeline UI widget — ported from https://github.com/alexeyvasilyev/timeline-ui-web
// (Apache-2.0). Converted to ES module, cleaned up: class syntax, no Date.prototype
// pollution, proper scoping, no leaked globals.

function formatHourMin(d) {
  const h = d.getHours(), m = d.getMinutes();
  return `${h < 10 ? "0" : ""}${h}:${m < 10 ? "0" : ""}${m}`;
}

function formatHourMinSec(d) {
  const h = d.getHours(), m = d.getMinutes(), s = d.getSeconds();
  return `${h < 10 ? "0" : ""}${h}:${m < 10 ? "0" : ""}${m}:${s < 10 ? "0" : ""}${s}`;
}

function formatShortDate(d) {
  return d.toLocaleString("en-us", { month: "short" }) + " " + d.getDate();
}

function isToday(d) {
  return new Date().toDateString() === d.toDateString();
}

class Rect {
  constructor(left, top, right, bottom, color) {
    this.left = left; this.top = top; this.right = right; this.bottom = bottom; this.color = color;
  }
}

class TimeRecord {
  constructor(timestampMsec, durationMsec, object, color) {
    this.timestampMsec = timestampMsec; this.durationMsec = durationMsec; this.object = object; this.color = color;
  }
}

const INTERVAL_MIN_1   =            60 * 1000;
const INTERVAL_MIN_5   =        5 * 60 * 1000;
const INTERVAL_MIN_15  =       15 * 60 * 1000;
const INTERVAL_MIN_30  =       30 * 60 * 1000;
const INTERVAL_HOUR_1  =       60 * 60 * 1000;
const INTERVAL_HOUR_6  =   6 * 60 * 60 * 1000;
const INTERVAL_HOUR_12 =  12 * 60 * 60 * 1000;
const INTERVAL_DAY_1   =  24 * 60 * 60 * 1000;
const INTERVAL_DAY_7   = 168 * 60 * 60 * 1000;
const INTERVAL_DAY_30  = 720 * 60 * 60 * 1000;
const OFFSET_TOP_BOTTOM = 25;

export class Timeline {
  constructor(options) {
    const date = new Date();
    this.options = options || {
      timelines: 1,
      colorTimeBackground: "#C62828",
      colorTimeText: "#FFFFFF",
    };
    this.resetRects();
    this.timelineSelected = 0;
    this.selectedMsec = date.getTime();
    this.intervalMsec = INTERVAL_HOUR_1;
    this.gmtOffsetInMillis = -date.getTimezoneOffset() * 60000;
    this.density = 1;
    this.canvas = null;
    this.requestMoreBackgroundDataCallback = null;
    this.requestMoreMajor1DataCallback = null;
    this.requestMoreMajor2DataCallback = null;
    this.timeSelectedCallback = null;
    this.liveCallback = null;
    this.timerSelectedId = 0;
    this._rafId = 0;   // pending coalesced draw (scheduleDraw)
    this._animRaf = 0; // in-flight easing animation (setCurrent/IntervalWithAnimation)
    this.isLive = false;
    this._hoverX = null;
    this._hoverMsec = null;
    this.recordsMajor1 = [];
    this.recordsMajor2 = [];
    this.recordsBackground = [];
    this.rectNoData = [];
    for (let i = 0; i < this.options.timelines; i++) {
      this.recordsMajor1[i] = [];
      this.recordsMajor2[i] = [];
      this.recordsBackground[i] = [];
      this.rectNoData[i] = null;
    }
  }

  setCurrentTimeline(timelineIndex) {
    if (timelineIndex >= 0 && timelineIndex < this.options.timelines)
      this.timelineSelected = timelineIndex;
  }

  getCurrentTimeline() { return this.timelineSelected; }
  getTotalTimelines() { return this.options.timelines; }

  setCurrent(selectedMsec) {
    this.selectedMsec = Math.min(selectedMsec, Date.now());
  }

  getCurrent() { return this.selectedMsec; }

  // scheduleDraw coalesces repaints to one per animation frame. Rapid callers
  // (wheel scroll, mousemove hover, the playback cursor tick) can call it freely
  // without triggering a full canvas rebuild per event — the browser paints at
  // most once per frame, and nothing runs while the tab is hidden.
  scheduleDraw() {
    if (this._rafId) return;
    this._rafId = requestAnimationFrame(() => {
      this._rafId = 0;
      this.draw();
    });
  }

  // _animate runs a 150ms easing loop on requestAnimationFrame, calling apply(p)
  // with p in [0,1]. It replaces the old 10ms setInterval (which repainted ~15
  // times regardless of the display refresh and kept ticking in a hidden tab).
  // Only one animation runs at a time (position vs zoom are mutually exclusive
  // user actions); starting a new one cancels the previous.
  _animate(apply) {
    const DURATION_MSEC = 150;
    if (this._animRaf) cancelAnimationFrame(this._animRaf);
    let t0 = null;
    const step = (ts) => {
      if (t0 === null) t0 = ts;
      const p = Math.min((ts - t0) / DURATION_MSEC, 1);
      apply(p);
      this.draw();
      this._animRaf = p < 1 ? requestAnimationFrame(step) : 0;
    };
    this._animRaf = requestAnimationFrame(step);
  }

  setCurrentWithAnimation(selectedMsec) {
    const from = this.selectedMsec;
    const to = Math.min(selectedMsec, Date.now());
    this._animate((p) => this.setCurrent(from + (to - from) * p));
  }

  setCanvas(canvas) { this.canvas = canvas; }

  setMajor1Records(timelineIndex, data) {
    if (timelineIndex > this.options.timelines - 1) {
      console.error(`Out of index [${timelineIndex}/${this.options.timelines - 1}] on setMajor1Records()`);
    } else {
      this.recordsMajor1[timelineIndex] = data;
    }
  }

  getMajor1Records(timelineIndex) { return this.recordsMajor1[timelineIndex]; }

  setMajor2Records(timelineIndex, data) {
    if (timelineIndex > this.options.timelines - 1) {
      console.error(`Out of index [${timelineIndex}/${this.options.timelines - 1}] on setMajor2Records()`);
    } else {
      this.recordsMajor2[timelineIndex] = data;
    }
  }

  getMajor2Records(timelineIndex) { return this.recordsMajor2[timelineIndex]; }

  setBackgroundRecords(timelineIndex, data) {
    if (timelineIndex > this.options.timelines - 1) {
      console.error(`Out of index [${timelineIndex}/${this.options.timelines - 1}] on setBackgroundRecords()`);
    } else {
      this.recordsBackground[timelineIndex] = data;
    }
  }

  getBackgroundRecords(timelineIndex) { return this.recordsBackground[timelineIndex]; }

  setRequestMoreBackgroundDataCallback(cb) { this.requestMoreBackgroundDataCallback = cb; }
  setRequestMoreMajor1DataCallback(cb) { this.requestMoreMajor1DataCallback = cb; }
  setRequestMoreMajor2DataCallback(cb) { this.requestMoreMajor2DataCallback = cb; }
  setTimeSelectedCallback(cb) { this.timeSelectedCallback = cb; }
  setLiveCallback(cb) { this.liveCallback = cb; }

  runTimeSelectedCallbackDelayed() {
    clearTimeout(this.timerSelectedId);
    this.timerSelectedId = setTimeout(() => {
      const timelineIndex = this.getCurrentTimeline();
      const record = this.getRecord(this.selectedMsec, this.recordsBackground[timelineIndex]);
      this.timerSelectedId = 0;
      this.timeSelectedCallback(timelineIndex, this.selectedMsec, record);
    }, 500);
  }

  getNextRecord(currentMsec, records) {
    let prevRecord = null;
    for (const record of records) {
      if (prevRecord != null && currentMsec < prevRecord.timestampMsec && currentMsec >= record.timestampMsec) {
        return prevRecord;
      }
      prevRecord = record;
    }
    return null;
  }

  getPrevRecord(currentMsec, records) {
    for (const record of records) {
      if (record.timestampMsec < currentMsec) return record;
    }
    return null;
  }

  getRecord(timestampMsec, records) {
    for (const record of records) {
      if (timestampMsec >= record.timestampMsec && timestampMsec < record.timestampMsec + record.durationMsec) {
        return record;
      }
    }
    return null;
  }

  // oldestTimestampMsec returns the smallest timestampMsec in a record list.
  // The lists are sorted, but in opposite directions (recordings ascending,
  // events descending), so the "request more into the past" trigger scans for
  // the true minimum instead of assuming which end holds the oldest record.
  oldestTimestampMsec(records) {
    let m = Infinity;
    for (const record of records) {
      if (record.timestampMsec < m) m = record.timestampMsec;
    }
    return m;
  }

  getNextMajorRecord(timelineIndex) {
    return this.getNextRecord(this.selectedMsec, this.recordsMajor1[timelineIndex]);
  }

  getPrevMajorRecord(timelineIndex) {
    return this.getPrevRecord(this.selectedMsec - 30000, this.recordsMajor1[timelineIndex]);
  }

  // msecFromPixel: maps an x pixel offset (0 = left edge) to the
  // corresponding wall-clock millisecond. Useful for hover tooltips.
  msecFromPixel(offsetX) {
    const msecInPixels = this.intervalMsec / this.canvas.clientWidth;
    const centerOffset = offsetX - this.canvas.clientWidth / 2;
    return this.selectedMsec + msecInPixels * centerOffset;
  }

  setHover(offsetX, msec) { this._hoverX = offsetX; this._hoverMsec = msec; }

  clearHover() { this._hoverX = null; this._hoverMsec = null; }

  drawCurrentTimeDate(context) {
    context.save();
    const date = new Date();
    date.setTime(this.selectedMsec);
    const isNow = Date.now() - this.selectedMsec < 1000;
    context.font = "12px sans-serif";
    const isLive = isNow && this.liveCallback != null;
    if (isLive || this.isLive) this.liveCallback(this.timelineSelected, isLive);
    this.isLive = isLive;
    let text = isLive ? "LIVE" : (isNow ? "Now" : formatHourMinSec(date));
    let textWidth = context.measureText(text).width;
    const textHeight = 12;
    let textX = this.canvas.clientWidth / 2 - textWidth / 2;
    context.fillStyle = this.options.colorTimeBackground;
    context.fillRect(textX - 2, 0, textWidth + 4, textHeight + 6);
    context.fillStyle = this.options.colorTimeText;
    context.fillText(text, textX, textHeight + 2);
    text = isToday(date) ? "Today" : date.toLocaleDateString();
    textWidth = context.measureText(text).width;
    textX = this.canvas.clientWidth - textWidth - 4;
    context.fillText(text, textX, textHeight + 2);
    context.restore();
  }

  getRulerScale() {
    if (this.intervalMsec > INTERVAL_DAY_30 - 1) return "30 days";
    if (this.intervalMsec > INTERVAL_DAY_7 - 1) return "7 days";
    if (this.intervalMsec > INTERVAL_DAY_1 - 1) return "1 day";
    if (this.intervalMsec > INTERVAL_HOUR_12 - 1) return "12 hours";
    if (this.intervalMsec > INTERVAL_HOUR_6 - 1) return "6 hours";
    if (this.intervalMsec > INTERVAL_HOUR_1 - 1) return "1 hour";
    if (this.intervalMsec > INTERVAL_MIN_30 - 1) return "30 min";
    if (this.intervalMsec > INTERVAL_MIN_15 - 1) return "15 min";
    if (this.intervalMsec > INTERVAL_MIN_5 - 1) return "5 min";
    if (this.intervalMsec > INTERVAL_MIN_1 - 1) return "1 min";
    return "";
  }

  drawHoursMinutes(context, msecInPixels, interval) {
    const minValue = this.selectedMsec - this.intervalMsec / 2 + this.gmtOffsetInMillis;
    const maxValue = this.selectedMsec + this.intervalMsec / 2 + this.gmtOffsetInMillis;
    if (minValue + this.selectedMsec % interval >= maxValue) return false;
    const numToDraw = this.intervalMsec / interval;
    const offsetInterval = minValue % interval;
    const textWidth = context.measureText("__00:00__").width;
    if (numToDraw * textWidth >= this.canvas.clientWidth) return false;
    const offsetInPixels = offsetInterval * msecInPixels;
    const curIntervalInPixels = this.intervalMsec * msecInPixels;
    const intervalInPixels = interval * msecInPixels;
    const height = this.canvas.clientHeight;
    const offsetTopBottom = OFFSET_TOP_BOTTOM * this.density;
    context.fillStyle = this.options.colorDigits;
    context.strokeStyle = this.options.colorDigits;
    context.save();
    context.beginPath();
    for (let i = 0; i < numToDraw; i++) {
      const startDate = new Date();
      startDate.setTime(minValue - offsetInterval + (i + 1) * interval - this.gmtOffsetInMillis);
      let text = formatHourMin(startDate);
      if (text === "00:00") {
        text = formatShortDate(startDate);
        context.font = "bold 10px sans-serif";
      } else {
        context.font = "10px sans-serif";
      }
      const x = curIntervalInPixels - (offsetInPixels + (numToDraw - i - 1) * intervalInPixels);
      const tw = context.measureText(text).width;
      context.fillText(text, x - tw / 2, height - offsetTopBottom / 1.7);
      context.moveTo(x, height - offsetTopBottom / 5);
      context.lineTo(x, height - offsetTopBottom / 2.5);
      const x2 = x - intervalInPixels / 2;
      if (x2 > 0) {
        context.moveTo(x2, height - offsetTopBottom / 5);
        context.lineTo(x2, height - offsetTopBottom / 4);
      }
    }
    context.stroke();
    context.restore();
    return true;
  }

  drawRuler(context) {
    const msecInPixels = this.canvas.clientWidth / this.intervalMsec;
    if (!this.drawHoursMinutes(context, msecInPixels, INTERVAL_MIN_1) &&
        !this.drawHoursMinutes(context, msecInPixels, INTERVAL_MIN_5) &&
        !this.drawHoursMinutes(context, msecInPixels, INTERVAL_MIN_15) &&
        !this.drawHoursMinutes(context, msecInPixels, INTERVAL_MIN_30) &&
        !this.drawHoursMinutes(context, msecInPixels, INTERVAL_HOUR_1) &&
        !this.drawHoursMinutes(context, msecInPixels, INTERVAL_HOUR_6) &&
        !this.drawHoursMinutes(context, msecInPixels, INTERVAL_HOUR_12) &&
        !this.drawHoursMinutes(context, msecInPixels, INTERVAL_DAY_1)) {
      this.drawHoursMinutes(context, msecInPixels, INTERVAL_DAY_7);
    }
    const textScale = this.getRulerScale();
    const tw = context.measureText(textScale).width;
    context.fillStyle = this.options.colorDigits;
    context.fillText(textScale, 4, 12);
  }

  onSingleTapUp(mouseEvent) {
    const offsetXInPixels = mouseEvent.offsetX - this.canvas.clientWidth / 2;
    const msecXInPixels = this.intervalMsec / this.canvas.clientWidth;
    const offsetXInMsec = msecXInPixels * offsetXInPixels;
    if (this.options.timelines > 0) {
      const y = mouseEvent.offsetY - OFFSET_TOP_BOTTOM;
      if (y > 0 && mouseEvent.offsetY < this.canvas.clientHeight - OFFSET_TOP_BOTTOM) {
        const timelineHeight = (this.canvas.clientHeight - OFFSET_TOP_BOTTOM * 2) / this.options.timelines;
        const timelineIndex = Math.floor(y / timelineHeight);
        if (this.timelineSelected !== timelineIndex) {
          this.timelineSelected = timelineIndex;
        }
      }
    }
    let newSelectedXMsec = this.selectedMsec + offsetXInMsec;
    let foundMajor2 = false;
    for (const record of this.recordsMajor2[this.timelineSelected]) {
      if (newSelectedXMsec >= record.timestampMsec && newSelectedXMsec <= record.timestampMsec + record.durationMsec) {
        newSelectedXMsec = record.timestampMsec;
        foundMajor2 = true;
        break;
      }
    }
    if (!foundMajor2) {
      let prevRecord = null;
      for (const record of this.recordsMajor1[this.timelineSelected]) {
        if (newSelectedXMsec >= record.timestampMsec && newSelectedXMsec <= record.timestampMsec + record.durationMsec) {
          newSelectedXMsec = record.timestampMsec;
          break;
        } else if (prevRecord != null && newSelectedXMsec > record.timestampMsec + record.durationMsec && newSelectedXMsec < prevRecord.timestampMsec) {
          newSelectedXMsec = prevRecord.timestampMsec;
          break;
        }
        prevRecord = record;
      }
    }
    this.setCurrentWithAnimation(newSelectedXMsec);
    this.runTimeSelectedCallbackDelayed();
  }

  onScroll(deltaX) {
    const msecInPixels = this.intervalMsec / this.canvas.clientWidth;
    this.setCurrent(this.getCurrent() - msecInPixels * deltaX);
    this.scheduleDraw();
    this.runTimeSelectedCallbackDelayed();
  }

  draw() {
    this.resetRects();
    for (let i = 0; i < this.options.timelines; i++) {
      this.updateRects(i);
    }
    const context = this.canvas.getContext("2d");
    const W = this.canvas.clientWidth;
    const H = this.canvas.clientHeight;
    context.fillStyle = this.options.colorBackground;
    context.fillRect(0, 0, W, H);
    for (let i = 0; i < this.options.timelines; i++) {
      if (this.options.timelines > 1 && i === this.timelineSelected) {
        context.fillStyle = this.options.colorTimelineSelected;
        context.fillRect(
          this.rectNoData[i].left - 2, this.rectNoData[i].top - 2,
          this.rectNoData[i].right - this.rectNoData[i].left + 4,
          this.rectNoData[i].bottom + 4,
        );
      }
      context.fillStyle = this.options.colorRectNoData;
      context.fillRect(
        this.rectNoData[i].left, this.rectNoData[i].top,
        this.rectNoData[i].right - this.rectNoData[i].left,
        this.rectNoData[i].bottom,
      );
      for (const rect of this.rectsBackground[i]) {
        context.fillStyle = rect.color === undefined ? this.options.colorRectBackground : rect.color;
        context.fillRect(rect.left, rect.top, rect.right - rect.left, rect.bottom);
      }
      for (const rect of this.rectsMajor1[i]) {
        context.fillStyle = rect.color === undefined ? this.options.colorRectMajor1 : rect.color;
        context.fillRect(rect.left, rect.top, rect.right - rect.left, rect.bottom);
      }
      if (this.rectMajor1Selected != null) {
        context.fillStyle = this.options.colorMajor1Selected;
        context.fillRect(
          this.rectMajor1Selected.left, this.rectMajor1Selected.top,
          this.rectMajor1Selected.right - this.rectMajor1Selected.left,
          this.rectMajor1Selected.bottom,
        );
      }
      for (const rect of this.rectsMajor2[i]) {
        context.fillStyle = rect.color === undefined ? this.options.colorRectMajor2 : rect.color;
        context.fillRect(rect.left, rect.top, rect.right - rect.left, rect.bottom);
      }
      if (this.rectMajor2Selected != null) {
        context.fillStyle = this.options.colorMajor2Selected;
        context.fillRect(
          this.rectMajor2Selected.left, this.rectMajor2Selected.top,
          this.rectMajor2Selected.right - this.rectMajor2Selected.left,
          this.rectMajor2Selected.bottom,
        );
      }
      context.fillStyle = this.options.colorTimeBackground;
      context.fillRect(W / 2, 0, 2, H);
      if (this.options.timelineNames != null && i < this.options.timelineNames.length) {
        const textHeight = 12;
        if (this.options.timelines > 1 && i === this.timelineSelected) {
          context.fillStyle = this.options.colorTimelineSelected;
          context.globalAlpha = 0.9;
        } else {
          context.fillStyle = this.options.colorBackground;
          context.globalAlpha = 0.6;
        }
        const tw = context.measureText(this.options.timelineNames[i]).width;
        const textX = 4;
        const textY = this.rectNoData[i].top + this.rectNoData[i].bottom / 2 + textHeight / 2;
        context.fillRect(textX - 2, textY - textHeight, tw + 4, textHeight + 6);
        context.globalAlpha = 1.0;
        context.fillStyle = this.options.colorTimeText;
        context.fillText(this.options.timelineNames[i], textX, textY);
      }
      this.drawRuler(context);
      this.drawCurrentTimeDate(context);
    }
    if (this._hoverMsec != null && this._hoverX != null) {
      this.drawHoverTooltip(context);
    }
  }

  drawHoverTooltip(context) {
    const d = new Date(this._hoverMsec);
    const text = d.toLocaleString();
    context.save();
    context.font = "11px sans-serif";
    const tw = context.measureText(text).width;
    const pad = 8;
    const boxW = tw + pad;
    const boxH = 20;
    let x = this._hoverX - boxW / 2;
    if (x < 2) x = 2;
    if (x + boxW > this.canvas.clientWidth - 2) x = this.canvas.clientWidth - boxW - 2;
    const y = this.canvas.clientHeight - boxH - 4;
    context.fillStyle = "rgba(0,0,0,0.85)";
    context.beginPath();
    context.roundRect(x, y, boxW, boxH, 4);
    context.fill();
    context.fillStyle = "#fff";
    context.textBaseline = "middle";
    context.fillText(text, x + pad / 2, y + boxH / 2);
    context.restore();
  }

  resetRects() {
    this.rectMajor1Selected = null;
    this.rectMajor2Selected = null;
    this.rectsMajor1 = [];
    this.rectsMajor2 = [];
    this.rectsBackground = [];
    for (let i = 0; i < this.options.timelines; i++) {
      this.rectsMajor1[i] = [];
      this.rectsMajor2[i] = [];
      this.rectsBackground[i] = [];
    }
  }

  updateRects(timelineIndex) {
    const offsetTopBottom = OFFSET_TOP_BOTTOM * this.density;
    const width = this.canvas.clientWidth;
    const laneH = (this.canvas.clientHeight - offsetTopBottom * 2) / this.options.timelines;
    const top = laneH * timelineIndex + offsetTopBottom;
    const minValue = this.selectedMsec - this.intervalMsec / 2;
    const maxValue = this.selectedMsec + this.intervalMsec / 2;
    const msecInPixels = width / this.intervalMsec;
    const offsetBackground = 2.0;
    const offsetMajor1 = offsetBackground;
    const offsetMajor2 = 4.0;
    this.rectNoData[timelineIndex] = new Rect(
      0, offsetMajor1 + top,
      Math.min((Date.now() - minValue) * msecInPixels, width),
      laneH - offsetMajor1 * 2,
    );
    for (const record of this.recordsMajor1[timelineIndex]) {
      if (record.timestampMsec + record.durationMsec >= minValue && record.timestampMsec <= maxValue) {
        const rect = new Rect(
          Math.max((record.timestampMsec - minValue) * msecInPixels, 0),
          offsetMajor1 + top,
          Math.min((record.timestampMsec - minValue + record.durationMsec) * msecInPixels, width),
          laneH - offsetMajor1 * 2,
          record.color,
        );
        if (rect.right - rect.left < 1) rect.right += 1;
        if (this.rectMajor1Selected == null && timelineIndex === this.timelineSelected &&
            this.selectedMsec >= record.timestampMsec && this.selectedMsec < record.timestampMsec + record.durationMsec) {
          this.rectMajor1Selected = rect;
        } else {
          this.rectsMajor1[timelineIndex].push(rect);
        }
      }
    }
    if (this.requestMoreMajor1DataCallback != null && this.recordsMajor1[timelineIndex].length > 0) {
      if (minValue < this.oldestTimestampMsec(this.recordsMajor1[timelineIndex])) {
        this.requestMoreMajor1DataCallback(timelineIndex);
      }
    }
    for (const record of this.recordsMajor2[timelineIndex]) {
      if (record.timestampMsec + record.durationMsec >= minValue && record.timestampMsec <= maxValue) {
        const rect = new Rect(
          Math.max((record.timestampMsec - minValue) * msecInPixels, 0),
          offsetMajor2 + top,
          Math.min((record.timestampMsec - minValue + record.durationMsec) * msecInPixels, width),
          laneH - offsetMajor2 * 2,
          record.color,
        );
        if (this.rectMajor2Selected == null && timelineIndex === this.timelineSelected &&
            this.selectedMsec >= record.timestampMsec && this.selectedMsec < record.timestampMsec + record.durationMsec) {
          this.rectMajor2Selected = rect;
        } else {
          this.rectsMajor2[timelineIndex].push(rect);
        }
      }
    }
    if (this.requestMoreMajor2DataCallback != null && this.recordsMajor2[timelineIndex].length > 0) {
      if (minValue < this.oldestTimestampMsec(this.recordsMajor2[timelineIndex])) {
        this.requestMoreMajor2DataCallback(timelineIndex);
      }
    }
    for (const record of this.recordsBackground[timelineIndex]) {
      if (record.timestampMsec + record.durationMsec >= minValue && record.timestampMsec <= maxValue) {
        const rect = new Rect(
          Math.max((record.timestampMsec - minValue) * msecInPixels, 0),
          offsetBackground + top,
          Math.min((record.timestampMsec - minValue + record.durationMsec) * msecInPixels, width),
          laneH - offsetBackground * 2,
          record.color,
        );
        if (rect.right - rect.left < 1) rect.right += 1;
        this.rectsBackground[timelineIndex].push(rect);
      }
    }
    if (this.requestMoreBackgroundDataCallback != null && this.recordsBackground[timelineIndex].length > 0) {
      if (minValue < this.oldestTimestampMsec(this.recordsBackground[timelineIndex])) {
        this.requestMoreBackgroundDataCallback(timelineIndex);
      }
    }
  }

  setIntervalWithAnimation(interval) {
    const from = this.intervalMsec;
    this._animate((p) => { this.intervalMsec = from + (interval - from) * p; });
  }

  getInterval() { return this.intervalMsec; }

  increaseInterval() {
    if (this.intervalMsec > INTERVAL_DAY_7 - 1) this.setIntervalWithAnimation(INTERVAL_DAY_30);
    else if (this.intervalMsec > INTERVAL_DAY_1 - 1) this.setIntervalWithAnimation(INTERVAL_DAY_7);
    else if (this.intervalMsec > INTERVAL_HOUR_12 - 1) this.setIntervalWithAnimation(INTERVAL_DAY_1);
    else if (this.intervalMsec > INTERVAL_HOUR_6 - 1) this.setIntervalWithAnimation(INTERVAL_HOUR_12);
    else if (this.intervalMsec > INTERVAL_HOUR_1 - 1) this.setIntervalWithAnimation(INTERVAL_HOUR_6);
    else if (this.intervalMsec > INTERVAL_MIN_30 - 1) this.setIntervalWithAnimation(INTERVAL_HOUR_1);
    else if (this.intervalMsec > INTERVAL_MIN_15 - 1) this.setIntervalWithAnimation(INTERVAL_MIN_30);
    else if (this.intervalMsec > INTERVAL_MIN_5 - 1) this.setIntervalWithAnimation(INTERVAL_MIN_15);
    else if (this.intervalMsec > INTERVAL_MIN_1 - 1) this.setIntervalWithAnimation(INTERVAL_MIN_5);
    else this.setIntervalWithAnimation(INTERVAL_MIN_1);
  }

  decreaseInterval() {
    if (this.intervalMsec > INTERVAL_DAY_30 - 1) this.setIntervalWithAnimation(INTERVAL_DAY_7);
    else if (this.intervalMsec > INTERVAL_DAY_7 - 1) this.setIntervalWithAnimation(INTERVAL_DAY_1);
    else if (this.intervalMsec > INTERVAL_DAY_1 - 1) this.setIntervalWithAnimation(INTERVAL_HOUR_12);
    else if (this.intervalMsec > INTERVAL_HOUR_12 - 1) this.setIntervalWithAnimation(INTERVAL_HOUR_6);
    else if (this.intervalMsec > INTERVAL_HOUR_6 - 1) this.setIntervalWithAnimation(INTERVAL_HOUR_1);
    else if (this.intervalMsec > INTERVAL_HOUR_1 - 1) this.setIntervalWithAnimation(INTERVAL_MIN_30);
    else if (this.intervalMsec > INTERVAL_MIN_30 - 1) this.setIntervalWithAnimation(INTERVAL_MIN_15);
    else if (this.intervalMsec > INTERVAL_MIN_15 - 1) this.setIntervalWithAnimation(INTERVAL_MIN_5);
    else if (this.intervalMsec > INTERVAL_MIN_5 - 1) this.setIntervalWithAnimation(INTERVAL_MIN_1);
  }
}
