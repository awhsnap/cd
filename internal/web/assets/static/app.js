// Calendar Dynamics dashboard. Talks to the Go API at /api/*.
(() => {
  "use strict";

  /* ---------------------------------------------------------- API client */
  async function request(method, path, body) {
    const opts = { method, credentials: "same-origin", headers: { "X-Requested-With": "calendar-dynamics" } };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    if (res.status === 401) {
      location.href = "/login";
      throw new Error("Signed out");
    }
    if (res.status === 204) return null;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "Something went wrong.");
    return data;
  }
  const enc = encodeURIComponent;
  const toEvent = e => ({ ...e, start: new Date(e.start) });

  const api = {
    me: () => request("GET", "/api/me"),
    listEvents: (from, to) =>
      request("GET", `/api/events?from=${enc(from.toISOString())}&to=${enc(to.toISOString())}`).then(l => l.map(toEvent)),
    getEvent: id => request("GET", `/api/events/${enc(id)}`).then(toEvent),
    saveEvent: (id, data) =>
      (id ? request("PUT", `/api/events/${enc(id)}`, data) : request("POST", "/api/events", data)).then(toEvent),
    deleteEvent: id => request("DELETE", `/api/events/${enc(id)}`),
    listContacts: q => request("GET", `/api/contacts?q=${enc(q || "")}`),
    getContact: id => request("GET", `/api/contacts/${enc(id)}`),
    saveContact: (id, data) => (id ? request("PUT", `/api/contacts/${enc(id)}`, data) : request("POST", "/api/contacts", data)),
    listContactEvents: id => request("GET", `/api/contacts/${enc(id)}/events`).then(l => l.map(toEvent)),
  };

  /* ---------------------------------------------------------- helpers */
  const DOW = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  const DAY_START = 7, DAY_END = 19, HOUR_PX = 52;
  const TYPES = { job: "Job", estimate: "Estimate", followup: "Follow-up", call: "Call", note: "Note" };
  const SEND_LABEL = { scheduled: "Will send", sent: "Sent", skipped: "Not sent", failed: "Failed" };

  const pad = n => String(n).padStart(2, "0");
  const startOfDay = d => { const x = new Date(d); x.setHours(0, 0, 0, 0); return x; };
  const addDays = (d, n) => { const x = new Date(d); x.setDate(x.getDate() + n); return x; };
  const startOfWeek = d => addDays(startOfDay(d), -new Date(d).getDay());
  const sameDay = (a, b) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  const ymd = d => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const parseYmd = s => { const [y, m, d] = s.split("-").map(Number); return new Date(y, m - 1, d); };
  const fmtTime = d => d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  const fmtCompact = d => `${d.getHours() % 12 || 12}${d.getMinutes() ? ":" + pad(d.getMinutes()) : ""}${d.getHours() < 12 ? "a" : "p"}`;
  const fmtShort = d => d.toLocaleDateString([], { month: "short", day: "numeric" });
  const fmtLong = d => d.toLocaleDateString([], { weekday: "short", month: "short", day: "numeric", year: d.getFullYear() !== new Date().getFullYear() ? "numeric" : undefined });
  const fmtDur = m => m >= 480 ? "Full day" : m >= 60 ? `${+(m / 60).toFixed(1)} hr` : `${m} min`;
  const hourLabel = h => `${h % 12 || 12}${h < 12 ? "a" : "p"}`;
  const initials = name => (name || "?").split(/\s+/).filter(Boolean).slice(0, 2).map(w => w[0].toUpperCase()).join("");
  const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  function relDays(d) {
    const diff = Math.round((startOfDay(d) - startOfDay(new Date())) / 86400000);
    if (diff === 0) return "Today";
    if (diff === 1) return "Tomorrow";
    if (diff === -1) return "Yesterday";
    return diff > 0 ? `in ${diff}d` : `${-diff}d ago`;
  }
  const fmtPhone = p => /^\+1\d{10}$/.test(p || "") ? `(${p.slice(2, 5)}) ${p.slice(5, 8)}-${p.slice(8)}` : (p || "");
  const $ = id => document.getElementById(id);

  let toastTimer;
  function toast(msg, isError) {
    const t = $("toast");
    t.textContent = msg;
    t.className = "toast" + (isError ? " error" : "");
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, isError ? 6000 : 3000);
  }
  const fail = err => { if (err && err.message !== "Signed out") toast(err.message || "Something went wrong.", true); };

  /* ---------------------------------------------------------- state */
  const state = {
    view: "month",
    cursor: startOfDay(new Date()),
    selected: startOfDay(new Date()),
    query: "",
    contacts: [],
    timelineContactId: null,
  };
  const calEl = $("calendar");

  $("legend").innerHTML = Object.entries(TYPES).map(([k, v]) => `<span><i class="${k}"></i>${v}</span>`).join("");
  $("typeSelect").innerHTML = Object.entries(TYPES).map(([k, v]) => `<option value="${k}">${v}</option>`).join("");

  /* ---------------------------------------------------------- month view */
  function chip(e) {
    return `<button class="chip ${e.type}" data-id="${esc(e.id)}" title="${esc(`${fmtTime(e.start)} · ${e.title} · ${e.contactName}`)}"><span class="t">${fmtCompact(e.start)}</span> ${esc(e.contactName)} · ${esc(e.title)}</button>`;
  }

  function renderMonth(evts, gridStart) {
    const month = state.cursor.getMonth();
    const today = startOfDay(new Date());
    let html = `<div class="dow">${DOW.map(d => `<div>${d}</div>`).join("")}</div><div class="month-grid">`;
    for (let i = 0; i < 42; i++) {
      const d = addDays(gridStart, i);
      const dayEvts = evts.filter(e => sameDay(e.start, d));
      const cls = ["cell", d.getMonth() !== month ? "muted" : "", sameDay(d, today) ? "today" : "", sameDay(d, state.selected) ? "selected" : ""].join(" ");
      const label = d.toLocaleDateString([], { weekday: "long", month: "long", day: "numeric" });
      html += `<div class="${cls}" data-date="${ymd(d)}" tabindex="0" role="button" aria-label="${label}, ${dayEvts.length} events">
        <div class="cell-head"><span class="num">${d.getDate()}</span><button class="add" data-add="${ymd(d)}" aria-label="Add event on ${label}">+</button></div>
        <div class="chips">${dayEvts.slice(0, 3).map(chip).join("")}${dayEvts.length > 3 ? `<div class="more">+${dayEvts.length - 3} more</div>` : ""}</div>
      </div>`;
    }
    return html + "</div>";
  }

  /* ---------------------------------------------------------- week view */
  function renderWeek(evts, weekStart) {
    const today = startOfDay(new Date());
    const days = Array.from({ length: 7 }, (_, i) => addDays(weekStart, i));
    const hours = []; for (let h = DAY_START; h < DAY_END; h++) hours.push(h);
    let html = `<div class="week-scroll"><div class="week" style="--hour:${HOUR_PX}px">`;
    html += `<div class="wk-head"><div></div>${days.map(d =>
      `<div class="wk-day ${sameDay(d, today) ? "today" : ""} ${sameDay(d, state.selected) ? "selected" : ""}" data-date="${ymd(d)}"><span>${DOW[d.getDay()]}</span><b>${d.getDate()}</b></div>`).join("")}</div>`;
    html += `<div class="wk-body"><div class="wk-times">${hours.map(h => `<div>${hourLabel(h)}</div>`).join("")}</div>`;
    html += days.map(d => {
      const slots = hours.map(h => `<div class="slot" data-date="${ymd(d)}" data-hour="${h}"></div>`).join("");
      const blocks = evts.filter(e => sameDay(e.start, d)).map(e => {
        const mins = e.start.getHours() * 60 + e.start.getMinutes() - DAY_START * 60;
        const top = Math.max(0, mins) / 60 * HOUR_PX;
        const height = Math.max(22, e.durationMins / 60 * HOUR_PX - 3);
        const short = height < 40 ? " short" : "";
        return `<button class="block ${e.type}${short}" data-id="${esc(e.id)}" title="${esc(`${fmtTime(e.start)} · ${e.contactName} · ${e.title}`)}" style="top:${top}px;height:${height}px"><b>${esc(e.contactName)}</b><span>${short ? fmtCompact(e.start) : fmtTime(e.start)} · ${esc(e.title)}</span></button>`;
      }).join("");
      return `<div class="wk-col">${slots}${blocks}</div>`;
    }).join("");
    return html + "</div></div></div>";
  }

  /* ---------------------------------------------------------- side panels */
  function sendBadge(e) {
    return e.type === "followup" && e.sendStatus ? `<span class="send-status ${e.sendStatus}">${SEND_LABEL[e.sendStatus]}</span>` : "";
  }

  async function renderAgenda() {
    const day = state.selected;
    const evts = await api.listEvents(day, addDays(day, 1));
    $("agendaTitle").textContent = day.toLocaleDateString([], { weekday: "long", month: "short", day: "numeric" });
    $("agendaTag").innerHTML = sameDay(day, new Date()) ? `<span class="tag-today">Today</span>` : "";
    $("agenda").innerHTML = evts.length
      ? evts.map(e => `<li class="item">
          <span class="when">${fmtTime(e.start)}</span>
          <span class="item-main">
            <button class="link what" data-id="${esc(e.id)}">${esc(e.title)}</button>
            <button class="link who" data-contact="${esc(e.contactId)}">${esc(e.contactName)}</button>
            <span><span class="badge ${e.type}">${TYPES[e.type]}</span>${sendBadge(e)}</span>
          </span>
        </li>`).join("")
      : `<li class="empty">Nothing scheduled.<br><button class="btn btn-ghost" style="margin-top:10px" data-add="${ymd(day)}">+ Add event</button></li>`;
  }

  async function loadContacts() {
    state.contacts = await api.listContacts(state.query);
    renderContacts();
  }

  function renderContacts() {
    const rows = [...state.contacts];
    rows.sort((a, b) =>
      (a.nextStart && b.nextStart) ? a.nextStart.localeCompare(b.nextStart) :
      a.nextStart ? -1 : b.nextStart ? 1 :
      (b.lastStart || "").localeCompare(a.lastStart || ""));
    $("contacts").innerHTML = rows.length
      ? rows.map(c => {
          const line = c.nextStart
            ? `<small class="next">Next · ${relDays(new Date(c.nextStart))} · ${esc(c.nextTitle)}</small>`
            : c.lastStart ? `<small>Last · ${fmtShort(new Date(c.lastStart))} · ${esc(c.lastTitle)}</small>` : `<small>No events yet</small>`;
          return `<li><button class="contact-row" data-contact="${esc(c.id)}"><span class="av">${esc(initials(c.name))}</span><span><b>${esc(c.name)}</b>${line}</span></button></li>`;
        }).join("")
      : state.query
        ? `<li class="empty">No contacts match “${esc(state.query)}”.</li>`
        : `<li class="empty">No contacts yet. Add one, or create an event and type a new name.</li>`;
  }

  async function renderStats() {
    const today = startOfDay(new Date());
    const now = new Date();
    const ws = startOfWeek(today);
    const [todayE, weekE, next7] = await Promise.all([
      api.listEvents(today, addDays(today, 1)),
      api.listEvents(ws, addDays(ws, 7)),
      api.listEvents(now, addDays(today, 8)),
    ]);
    $("statToday").textContent = todayE.filter(e => e.type === "job").length;
    $("statWeek").textContent = weekE.length;
    $("statFollow").textContent = next7.filter(e => e.type === "followup").length;
    const cutoff = addDays(today, -90).toISOString();
    $("statActive").textContent = state.query ? "–" : state.contacts.filter(c => c.nextStart || (c.lastStart && c.lastStart >= cutoff)).length;
  }

  /* ---------------------------------------------------------- timeline drawer */
  const drawer = $("drawer"), drawerScrim = $("drawerScrim");

  async function openTimeline(contactId) {
    state.timelineContactId = contactId;
    try {
      await renderTimeline();
    } catch (err) { fail(err); return; }
    drawer.classList.add("open");
    drawerScrim.classList.add("open");
    drawer.setAttribute("aria-hidden", "false");
    $("drawerClose").focus();
  }

  function closeTimeline() {
    state.timelineContactId = null;
    drawer.classList.remove("open");
    drawerScrim.classList.remove("open");
    drawer.setAttribute("aria-hidden", "true");
  }

  function tlItem(e, past) {
    const note = e.type === "followup" && e.sendNote ? `<p class="tl-notes">${esc(e.sendNote)}</p>` : "";
    const body = e.type === "followup" && e.message ? e.message : e.notes;
    return `<li class="tl-item t-${e.type} ${past ? "past" : ""}">
      <button data-id="${esc(e.id)}" data-jump="1">
        <div class="tl-date">${fmtLong(e.start)} · ${fmtTime(e.start)} <span style="font-weight:600;opacity:.75">(${relDays(e.start)})</span></div>
        <div class="tl-title">${esc(e.title)}</div>
        <div class="tl-meta"><span class="badge ${e.type}">${TYPES[e.type]}</span>${e.type === "note" ? "" : fmtDur(e.durationMins)}${sendBadge(e)}</div>
        ${body ? `<p class="tl-notes">${esc(body)}</p>` : ""}${note}
      </button>
    </li>`;
  }

  async function renderTimeline() {
    const id = state.timelineContactId;
    if (!id) return;
    const [c, evts] = await Promise.all([api.getContact(id), api.listContactEvents(id)]);
    const now = new Date();
    const upcoming = evts.filter(e => e.start >= now);
    const past = evts.filter(e => e.start < now).reverse();

    $("tlAvatar").textContent = initials(c.name);
    $("tlName").textContent = c.name;
    $("tlSince").textContent = evts.length
      ? `Customer since ${evts[0].start.toLocaleDateString([], { month: "short", day: "numeric", year: "numeric" })} · ${evts.length} events`
      : "New contact";
    $("tlMeta").innerHTML = [
      c.phone && `<a href="tel:${esc(c.phone)}">📞 ${esc(fmtPhone(c.phone))}</a>`,
      c.email && `<a href="mailto:${esc(c.email)}">✉️ ${esc(c.email)}</a>`,
      c.address && `<span>📍 ${esc(c.address)}</span>`,
      c.optedOut ? `<span class="optout">Opted out of texts</span>` : c.phone && !c.smsConsent ? `<span>No texting consent on file</span>` : "",
    ].filter(Boolean).join("");
    $("tlJobs").textContent = evts.filter(e => e.type === "job").length;
    $("tlLast").textContent = past[0] ? relDays(past[0].start) : "—";
    $("tlNext").textContent = upcoming[0] ? relDays(upcoming[0].start) : "—";

    let html = "";
    if (upcoming.length) html += `<div class="tl-group">Upcoming · ${upcoming.length}</div><ul class="tl">${upcoming.map(e => tlItem(e, false)).join("")}</ul>`;
    html += `<div class="tl-group tl-now">Now</div>`;
    html += past.length
      ? `<div class="tl-group">History · ${past.length}</div><ul class="tl">${past.map(e => tlItem(e, true)).join("")}</ul>`
      : `<p class="empty">No history yet. Log a call or note to start the timeline.</p>`;
    $("tlBody").innerHTML = html;
  }

  /* ---------------------------------------------------------- main render */
  let renderSeq = 0;
  async function render() {
    const seq = ++renderSeq;
    let from, to;
    if (state.view === "month") {
      const first = new Date(state.cursor.getFullYear(), state.cursor.getMonth(), 1);
      from = startOfWeek(first); to = addDays(from, 42);
      $("calTitle").textContent = first.toLocaleDateString([], { month: "long", year: "numeric" });
    } else {
      from = startOfWeek(state.cursor); to = addDays(from, 7);
      $("calTitle").textContent = `${fmtShort(from)} – ${addDays(from, 6).toLocaleDateString([], { month: "short", day: "numeric", year: "numeric" })}`;
    }
    document.querySelectorAll(".seg button").forEach(b => b.setAttribute("aria-pressed", String(b.dataset.view === state.view)));
    try {
      const evts = await api.listEvents(from, to);
      if (seq !== renderSeq) return; // a newer render started
      calEl.innerHTML = state.view === "month" ? renderMonth(evts, from) : renderWeek(evts, from);
      await loadContacts();
      await Promise.all([renderAgenda(), renderStats(), renderTimeline()]);
    } catch (err) { fail(err); }
  }

  /* ---------------------------------------------------------- event dialog */
  const dlg = $("eventDialog"), form = $("eventForm");
  let editing = null;

  function showFollowupFields() {
    const isFollowup = form.type.value === "followup";
    document.querySelectorAll("#eventForm .followup-only").forEach(el => { el.hidden = !isFollowup; });
  }
  form.type.addEventListener("change", showFollowupFields);

  function setError(el, msg) { el.textContent = msg || ""; el.hidden = !msg; }

  async function openDialog({ event = null, dateStr, hour, type, contactId } = {}) {
    form.reset();
    setError($("eventError"), "");
    editing = event;
    $("dlgTitle").textContent = event ? `Edit ${TYPES[event.type].toLowerCase()}` : "New event";
    $("deleteBtn").hidden = !event;
    $("timelineBtn").hidden = !event;
    $("contactOptions").innerHTML = state.contacts.map(c => `<option value="${esc(c.name)}"></option>`).join("");

    let start;
    if (event) start = new Date(event.start);
    else if (type === "call" || type === "note") start = new Date();
    else { start = dateStr ? parseYmd(dateStr) : new Date(state.selected); start.setHours(hour ?? 9, 0, 0, 0); }

    let presetName = event ? event.contactName : "";
    if (!event && contactId) {
      try { presetName = (await api.getContact(contactId)).name; } catch (_) { /* ignore */ }
    }
    const t = event ? event.type : (type || "job");
    form.contact.value = presetName;
    form.type.value = t;
    form.title.value = event ? event.title : "";
    form.date.value = ymd(start);
    form.time.value = `${pad(start.getHours())}:${pad(start.getMinutes())}`;
    form.duration.value = String(event ? event.durationMins : (t === "job" ? 120 : t === "estimate" ? 60 : 15));
    form.notes.value = event ? event.notes : "";
    form.channel.value = event ? event.channel : (t === "followup" ? "sms" : "");
    form.message.value = event ? event.message : "";
    showFollowupFields();
    dlg.showModal();
    (presetName ? form.title : form.contact).focus();
  }

  form.addEventListener("submit", async e => {
    e.preventDefault();
    const [hh, mm] = form.time.value.split(":").map(Number);
    const start = parseYmd(form.date.value);
    start.setHours(hh, mm, 0, 0);
    const name = form.contact.value.trim();
    const sameContact = editing && name.toLowerCase() === editing.contactName.toLowerCase();
    const payload = {
      contactId: sameContact ? editing.contactId : "",
      contactName: name,
      type: form.type.value,
      title: form.title.value.trim(),
      start: start.toISOString(),
      durationMins: Number(form.duration.value),
      notes: form.notes.value.trim(),
      channel: form.type.value === "followup" ? form.channel.value : "",
      message: form.type.value === "followup" ? form.message.value.trim() : "",
    };
    form.classList.add("loading");
    try {
      await api.saveEvent(editing ? editing.id : "", payload);
      state.selected = startOfDay(start);
      state.cursor = startOfDay(start);
      dlg.close();
      toast(payload.channel ? "Saved — it will send automatically." : "Saved.");
      render();
    } catch (err) {
      setError($("eventError"), err.message);
    } finally {
      form.classList.remove("loading");
    }
  });

  $("deleteBtn").addEventListener("click", async () => {
    if (!editing || !confirm("Delete this event?")) return;
    try {
      await api.deleteEvent(editing.id);
      dlg.close();
      toast("Deleted.");
      render();
    } catch (err) { setError($("eventError"), err.message); }
  });
  $("timelineBtn").addEventListener("click", () => {
    const id = editing && editing.contactId;
    dlg.close();
    if (id) openTimeline(id);
  });

  /* ---------------------------------------------------------- contact dialog */
  const cdlg = $("contactDialog"), cform = $("contactForm");
  let editingContact = null;

  async function openContactDialog(contactId) {
    cform.reset();
    setError($("contactError"), "");
    editingContact = null;
    if (contactId) {
      try { editingContact = await api.getContact(contactId); } catch (err) { fail(err); return; }
    }
    $("contactDlgTitle").textContent = editingContact ? "Edit contact" : "New contact";
    const c = editingContact || {};
    cform.name.value = c.name || "";
    cform.phone.value = fmtPhone(c.phone);
    cform.email.value = c.email || "";
    cform.address.value = c.address || "";
    cform.smsConsent.checked = !!c.smsConsent;
    cdlg.showModal();
    cform.name.focus();
  }

  cform.addEventListener("submit", async e => {
    e.preventDefault();
    const data = {
      name: cform.name.value.trim(), phone: cform.phone.value.trim(), email: cform.email.value.trim(),
      address: cform.address.value.trim(), smsConsent: cform.smsConsent.checked,
    };
    try {
      const saved = await api.saveContact(editingContact ? editingContact.id : "", data);
      cdlg.close();
      toast("Contact saved.");
      if (!editingContact) state.timelineContactId = saved.id;
      await render();
      if (!editingContact) openTimeline(saved.id);
    } catch (err) { setError($("contactError"), err.message); }
  });

  document.querySelectorAll("dialog [data-close]").forEach(b => b.addEventListener("click", () => b.closest("dialog").close()));

  /* ---------------------------------------------------------- interactions */
  async function handleClick(e) {
    const contactBtn = e.target.closest("[data-contact]");
    if (contactBtn) { openTimeline(contactBtn.dataset.contact); return; }

    const evBtn = e.target.closest("[data-id]");
    if (evBtn) {
      try {
        const evt = await api.getEvent(evBtn.dataset.id);
        if (evBtn.dataset.jump) {
          state.selected = startOfDay(evt.start);
          state.cursor = startOfDay(evt.start);
          render();
        }
        openDialog({ event: evt });
      } catch (err) { fail(err); }
      return;
    }
    const add = e.target.closest("[data-add]");
    if (add) { openDialog({ dateStr: add.dataset.add }); return; }
    const slot = e.target.closest(".slot");
    if (slot) { openDialog({ dateStr: slot.dataset.date, hour: Number(slot.dataset.hour) }); return; }
    const day = e.target.closest("[data-date]");
    if (day) selectDay(parseYmd(day.dataset.date));
  }

  function selectDay(d) {
    state.selected = d;
    if (state.view === "month" && d.getMonth() !== state.cursor.getMonth()) state.cursor = d;
    render();
  }

  [calEl, $("agenda"), $("contacts"), $("tlBody")].forEach(el => el.addEventListener("click", handleClick));
  calEl.addEventListener("keydown", e => {
    const cell = e.target.closest(".cell");
    if (cell && e.target === cell && (e.key === "Enter" || e.key === " ")) {
      e.preventDefault();
      selectDay(parseYmd(cell.dataset.date));
    }
  });

  document.querySelectorAll("[data-log]").forEach(b => b.addEventListener("click", () =>
    openDialog({ type: b.dataset.log, contactId: state.timelineContactId, dateStr: ymd(addDays(new Date(), b.dataset.log === "followup" ? 2 : 0)) })));
  $("editContactBtn").addEventListener("click", () => openContactDialog(state.timelineContactId));
  $("newContactBtn").addEventListener("click", () => openContactDialog(null));
  $("drawerClose").addEventListener("click", closeTimeline);
  drawerScrim.addEventListener("click", closeTimeline);
  document.addEventListener("keydown", e => {
    if (e.key === "Escape" && !dlg.open && !cdlg.open && drawer.classList.contains("open")) closeTimeline();
  });

  $("prevBtn").addEventListener("click", () => shift(-1));
  $("nextBtn").addEventListener("click", () => shift(1));
  function shift(dir) {
    const c = state.cursor;
    state.cursor = state.view === "month" ? new Date(c.getFullYear(), c.getMonth() + dir, 1) : addDays(c, 7 * dir);
    render();
  }
  $("todayBtn").addEventListener("click", () => { state.cursor = state.selected = startOfDay(new Date()); render(); });
  document.querySelectorAll(".seg button").forEach(b => b.addEventListener("click", () => {
    state.view = b.dataset.view;
    state.cursor = new Date(state.selected);
    render();
  }));
  $("newBtnTop").addEventListener("click", () => openDialog({ dateStr: ymd(state.selected) }));
  $("newBtnCal").addEventListener("click", () => openDialog({ dateStr: ymd(state.selected) }));

  let searchTimer;
  $("search").addEventListener("input", e => {
    state.query = e.target.value;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => loadContacts().catch(fail), 200);
  });
  $("search").addEventListener("keydown", e => {
    if (e.key === "Enter") {
      const first = $("contacts").querySelector("[data-contact]");
      if (first) openTimeline(first.dataset.contact);
    }
  });
  $("navContacts").addEventListener("click", () => {
    toggleNav(false);
    $("contactsCard").scrollIntoView({ behavior: "smooth", block: "start" });
    $("search").focus();
  });

  const sidebar = $("sidebar"), scrim = $("scrim");
  function toggleNav(open) { sidebar.classList.toggle("open", open); scrim.classList.toggle("open", open); }
  $("menuBtn").addEventListener("click", () => toggleNav(true));
  scrim.addEventListener("click", () => toggleNav(false));

  /* ---------------------------------------------------------- boot */
  const h = new Date().getHours();
  $("greeting").textContent = `${h < 12 ? "Good morning" : h < 17 ? "Good afternoon" : "Good evening"} — here's what's on the books.`;

  api.me().then(me => {
    $("bizName").textContent = me.business.name;
    $("userName").textContent = me.user.name || me.user.email;
    $("avatar").textContent = initials(me.user.name || me.user.email);
    document.title = `${me.business.name} · Calendar Dynamics`;
    if (me.supportAccess && me.supportAccess.length) {
      const a = me.supportAccess[0];
      const banner = $("supportBanner");
      banner.textContent = `Calendar Dynamics support (${a.staffName}) accessed your account on ${new Date(a.createdAt).toLocaleString()} — reason: ${a.reason}`;
      banner.hidden = false;
    }
  }).catch(fail);

  render();
})();
