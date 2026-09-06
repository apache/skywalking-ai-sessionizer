var __defProp = Object.defineProperty;
var __defNormalProp = (obj, key, value) => key in obj ? __defProp(obj, key, { enumerable: true, configurable: true, writable: true, value }) : obj[key] = value;
var __publicField = (obj, key, value) => __defNormalProp(obj, typeof key !== "symbol" ? key + "" : key, value);
function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#039;" })[c]);
}
function cssEscape(v) {
  var _a;
  const g = globalThis;
  if ((_a = g.CSS) == null ? void 0 : _a.escape) return g.CSS.escape(v);
  return v.replace(/["\\]/g, "\\$&");
}
function scrollTo(el, opts) {
  if (!el) return;
  const target = el;
  if (typeof target.scrollTo === "function") target.scrollTo(opts);
  else {
    if (opts.left !== void 0) target.scrollLeft = opts.left;
    if (opts.top !== void 0) target.scrollTop = opts.top;
  }
}
function reducedMotion() {
  const g = globalThis;
  return typeof g.matchMedia === "function" && g.matchMedia("(prefers-reduced-motion: reduce)").matches;
}
const UNOBSERVED = "--:--:--";
function makeFormatter(locale, unavailable = "unavailable") {
  const time = new Intl.DateTimeFormat(locale, { hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" });
  const short = new Intl.DateTimeFormat(locale, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
  const day = new Intl.DateTimeFormat(locale, { month: "short", day: "2-digit" });
  const num = new Intl.NumberFormat(locale);
  return {
    time: (ms) => ms ? time.format(new Date(ms)) : UNOBSERVED,
    timeShort: (ms) => ms ? short.format(new Date(ms)) : "--:--",
    day: (ms) => ms ? day.format(new Date(ms)) : "",
    dateTime: (ms) => ms ? `${day.format(new Date(ms))} ${time.format(new Date(ms))}` : UNOBSERVED,
    number: (n) => num.format(n ?? 0),
    duration: (ms) => {
      if (!Number.isFinite(ms)) return unavailable;
      if (ms < 1e3) return `${Math.round(ms)} ms`;
      if (ms < 6e4) return `${(ms / 1e3).toFixed(1)} s`;
      if (ms < 36e5) return `${Math.round(ms / 6e4)} min`;
      if (ms < 864e5) return `${(ms / 36e5).toFixed(1)} h`;
      return `${(ms / 864e5).toFixed(ms < 864e6 ? 1 : 0)} d`;
    }
  };
}
const EN_US_FORMATTER = makeFormatter("en-US");
const KIND = {
  "message.external": { track: "input", type: "user", title: "legendInput" },
  "message.assistant": { track: "messages", type: "assistant", title: "legendResponse" },
  "message.synthetic": { track: "messages", type: "error", title: "clientMadeMessage" },
  "context.injection": { track: "context", type: "instruction", title: "contextPutIn" },
  "agent.output": { track: "messages", type: "subagent-response", title: "streamOutput" },
  "llm.call": { track: "model", type: "model", title: "modelCall" },
  thinking: { track: "model", type: "model", title: "reasoning" },
  tool: { track: "tools", type: "tool", title: "toolWord" },
  "agent.call": { track: "agents", type: "agent", title: "agentCallToChild" },
  "agent.launch_ack": { track: "agents", type: "agent", title: "launchAcknowledged" },
  "runtime.notification": { track: "agents", type: "agent", title: "notificationFromChild" },
  "epoch.boundary": { track: "annotation", type: "annotation", title: null, raw: "Context reset" },
  "epoch.summary": { track: "annotation", type: "annotation", title: null, raw: "Carried-forward summary" },
  "turn.duration": { track: "annotation", type: "annotation", title: null, raw: "Turn duration" },
  "error.api": { track: "annotation", type: "error", title: null, raw: "Provider error" },
  "control.interrupt": { track: "annotation", type: "error", title: null, raw: "Interruption" },
  "control.permission": { track: "annotation", type: "annotation", title: null, raw: "Permission decision" },
  "control.command": { track: "annotation", type: "annotation", title: null, raw: "Runtime command" },
  "control.notice": { track: "annotation", type: "annotation", title: null, raw: "Runtime notice" }
};
function kindOf(kind) {
  return KIND[kind] ?? { track: "annotation", type: "annotation", title: null, raw: kind };
}
function kindTitle(kind, s) {
  const m = kindOf(kind);
  return m.title ? s[m.title] : m.raw ?? kind;
}
const CONTAINER_KINDS = /* @__PURE__ */ new Set(["talk", "run", "stream", "segment", "session", "epoch"]);
const TRACKS = ["input", "messages", "context", "model", "tools", "agents", "annotation"];
const TRACK_NAME = {
  input: "laneInput",
  messages: "laneResponses",
  context: "laneContext",
  model: "laneModel",
  tools: "laneTools",
  agents: "laneAgents",
  annotation: "laneNotices"
};
const INJECTION = {
  total_tokens: "the remaining token budget",
  todo_reminder: "the task list, restated",
  task_reminder: "a reminder about the running task",
  edited_text_file: "a file changed on disk",
  queued_command: "a message sent while the agent was working",
  ultra_effort_enter: "effort mode turned on",
  ultra_effort_exit: "effort mode turned off",
  deferred_tools_delta: "the tool list changed",
  agent_listing_delta: "the list of agents changed",
  nested_memory: "memory loaded from another file",
  date_change: "the date changed",
  command_permissions: "a permission decision",
  compact_file_reference: "a pointer left behind by compaction",
  read_truncation_notice: "a file read was cut short",
  invoked_skills: "a skill was loaded",
  auto_mode: "auto mode changed",
  file: "a file was attached",
  system_reminder: "a reminder from the harness",
  command_message: "a command was run in line"
};
function injectionKind(text) {
  const t = text.trim();
  const m = /^\{"type"\s*:\s*"([A-Za-z_]+)"/.exec(t);
  if (m) return m[1];
  const tag = /^<([a-z_-]+)>/.exec(t);
  if (tag) return tag[1].replace(/-/g, "_");
  return null;
}
function injectionSays(text) {
  if (!text) return null;
  const k = injectionKind(text);
  if (k) return { key: k, says: INJECTION[k] ?? k.replace(/_/g, " ") };
  const first = text.trim().split("\n").find((l) => l.trim());
  if (!first) return null;
  return { key: "", says: first.replace(/^[-*#\s]+/, "").slice(0, 60) };
}
const QUIET_MS = 120 * 1e3;
const LANE_H = 36;
function readOrder(a, b) {
  if (a.ref && b.ref) return a.ref.seq - b.ref.seq || a.ref.row - b.ref.row;
  return (a.at || 0) - (b.at || 0) || a.order - b.order;
}
function timeOrder(a, b) {
  return (a.at || 0) - (b.at || 0) || a.order - b.order;
}
class ConversationModel {
  constructor(doc) {
    __publicField(this, "talks", []);
    __publicField(this, "talkById", /* @__PURE__ */ new Map());
    __publicField(this, "streamByName", /* @__PURE__ */ new Map());
    __publicField(this, "streamById", /* @__PURE__ */ new Map());
    __publicField(this, "segmentById", /* @__PURE__ */ new Map());
    __publicField(this, "stepById", /* @__PURE__ */ new Map());
    /** Every step of a stream, in reading order. */
    __publicField(this, "stepsByStream", /* @__PURE__ */ new Map());
    /** The same steps, in time order, for the axis. */
    __publicField(this, "flowByStream", /* @__PURE__ */ new Map());
    __publicField(this, "talksByStream", /* @__PURE__ */ new Map());
    __publicField(this, "stepsByTalk", /* @__PURE__ */ new Map());
    /** Steps outside any talk, per stream. */
    __publicField(this, "looseByStream", /* @__PURE__ */ new Map());
    __publicField(this, "folderCache", /* @__PURE__ */ new Map());
    /** Steps that start or report a stream, keyed by the stream id. */
    __publicField(this, "openersByStreamId", /* @__PURE__ */ new Map());
    this.doc = doc;
    for (const s of doc.streams) {
      this.streamByName.set(s.name, s);
      this.streamById.set(s.id, s);
    }
    for (const seg of doc.segments) this.segmentById.set(seg.id, seg);
    let order = 0;
    const flatten = (root, talkId, streamFallback) => {
      const walk = (n, run, depth) => {
        if (n.kind === "run") run = n.id;
        if (!CONTAINER_KINDS.has(n.kind)) {
          const meta = kindOf(n.kind);
          const step = {
            id: n.id,
            kind: n.kind,
            at: n.at,
            parent: n.parent,
            stream: n.stream || streamFallback,
            run,
            talk: talkId,
            track: meta.track,
            type: meta.type,
            name: n.name,
            text: n.text,
            state: n.state,
            bytes: n.bytes,
            result: n.result,
            resultState: n.result_state,
            resultBytes: n.result_bytes,
            durationMs: n.duration_ms,
            durationHow: n.duration_measured_by,
            reqToRes: n.request_to_result_ms,
            reqToResJoin: n.request_to_result_join,
            failed: n.failed,
            ref: n.ref,
            refs: n.refs,
            attrs: n.attrs,
            usage: n.usage,
            flags: n.flags,
            dropped: n.dropped,
            edges: n.edges ?? [],
            order: order++,
            depth
          };
          this.stepById.set(step.id, step);
          push(this.stepsByStream, step.stream, step);
          if (talkId) push(this.stepsByTalk, talkId, step);
          else push(this.looseByStream, step.stream, step);
        }
        for (const k of n.children ?? []) walk(k, run, depth + 1);
      };
      walk(root, null, 0);
    };
    for (const t of doc.talks) {
      const row = {
        id: t.id,
        stream: t.stream ?? "main",
        label: t.label ?? "",
        reply: t.reply ?? "",
        runs: t.runs ?? 0,
        steps: t.steps ?? 0,
        tools: t.tools ?? 0,
        from: t.from ?? 0,
        to: t.to ?? 0,
        child: !!t.child,
        segment: t.segment ?? ""
      };
      this.talks.push(row);
      this.talkById.set(row.id, row);
      push(this.talksByStream, row.stream, row);
      flatten(t, t.id, row.stream);
    }
    for (const n of doc.loose ?? []) flatten(n, null, n.stream ?? "main");
    for (const [name, steps] of this.stepsByStream) {
      steps.sort(readOrder);
      this.flowByStream.set(name, [...steps].sort(timeOrder));
    }
    for (const steps of this.stepsByTalk.values()) steps.sort(readOrder);
    for (const steps of this.looseByStream.values()) steps.sort(readOrder);
    for (const step of this.stepById.values()) {
      for (const e of step.edges) {
        if (e.dir === "out" && (e.type === "starts" || e.type === "reports") && this.streamById.has(e.other)) {
          push(this.openersByStreamId, e.other, step);
        }
      }
    }
  }
  get title() {
    return this.doc.summary.title;
  }
  /** Steps of a stream in reading order. */
  steps(stream) {
    return this.stepsByStream.get(stream) ?? [];
  }
  /** Steps of a stream in time order, for the axis. */
  flow(stream) {
    return this.flowByStream.get(stream) ?? [];
  }
  talksOf(stream) {
    return this.talksByStream.get(stream) ?? [];
  }
  stepsOfTalk(talk) {
    return this.stepsByTalk.get(talk) ?? [];
  }
  loose(stream) {
    return this.looseByStream.get(stream) ?? [];
  }
  step(id) {
    return id ? this.stepById.get(id) ?? null : null;
  }
  /** The streams the assembler could tie to the start or the end of a stream
   *  from THIS stream's steps. Several candidates for one call are all kept:
   *  the assembler did not choose, and neither does a view. */
  foldersFor(stream) {
    const cached = this.folderCache.get(stream);
    if (cached) return cached;
    const seen = /* @__PURE__ */ new Map();
    const steps = this.steps(stream);
    for (const e of steps) {
      for (const g of e.edges) {
        if (g.dir !== "out" || g.type !== "starts" && g.type !== "reports") continue;
        const st = this.streamById.get(g.other);
        if (!st) continue;
        const prev = seen.get(st.name);
        if (prev && !prev.back) continue;
        seen.set(st.name, { stream: st, from: e, quality: g.quality, candidates: 0, back: g.type === "reports" });
      }
    }
    const calls = /* @__PURE__ */ new Map();
    for (const e of steps) {
      for (const g of e.edges) if (g.type === "starts" && g.dir === "out") calls.set(e.id, (calls.get(e.id) ?? 0) + 1);
    }
    const out = [...seen.values()];
    for (const f of out) f.candidates = calls.get(f.from.id) ?? 1;
    this.folderCache.set(stream, out);
    return out;
  }
  /** The step that opened a stream, from the stream's own `opened_by`. */
  openerOf(streamName2) {
    var _a;
    const st = this.streamByName.get(streamName2);
    const o = (_a = st == null ? void 0 : st.opened_by) == null ? void 0 : _a[0];
    if (!(o == null ? void 0 : o.step)) return null;
    const step = this.step(o.step);
    if (!step) return null;
    return { step, talk: o.talk || step.talk, quality: o.quality };
  }
  /** The streams a stream opened, for the "opens in turn" list. */
  openedBy(streamName2) {
    return this.doc.streams.filter((x) => (x.opened_by ?? []).some((o) => o.stream === streamName2));
  }
  /** The talk a step belongs to, walking up if the step is a container id. */
  talkOf(id) {
    const s = this.step(id);
    if (s) return s.talk;
    return this.talkById.has(id) ? id : null;
  }
  /** The first talk of the main stream, else the first talk at all. */
  firstTalk() {
    return this.talks.find((t) => !t.child) ?? this.talks[0] ?? null;
  }
  /** Everything that has to do with a selection: the step, what a relation
   *  joins it to, its owner, and what it owns. Null means nothing is picked and
   *  nothing fades. */
  relatedTo(sel, folder, streamSteps) {
    if (!sel) return null;
    if (folder) return /* @__PURE__ */ new Set([sel]);
    const here = this.step(sel);
    if (!here) return null;
    const set = /* @__PURE__ */ new Set([sel]);
    for (const r of here.edges) set.add(r.other);
    if (here.parent) set.add(here.parent);
    for (const e of streamSteps) if (e.parent === sel) set.add(e.id);
    return set;
  }
}
function push(m, k, v) {
  const list = m.get(k);
  if (list) list.push(v);
  else m.set(k, [v]);
}
const ENGLISH = {
  round: "round",
  segments: "segments",
  segmentList: "Segments",
  streams: "streams",
  talks: "talks",
  talkList: "Talks",
  span: "span",
  unresolved: "unresolved",
  overview: "Overview",
  session: "Session",
  steps: "Steps",
  runs: "runs",
  childStreams: "Child streams",
  childStreamsNote: "subagents, each its own context",
  relations: "Relations",
  activityWindows: "activity windows",
  idle: "idle",
  integrityVerified: "verified",
  integrityIncomplete: "incomplete",
  integrityMismatch: "mismatch",
  problems: "problems",
  roundsVerified: "rounds verified",
  filesListed: "files",
  filterTalks: "filter",
  noTalkMatches: "No talk matches.",
  untitled: "(untitled)",
  talk: "Talk",
  mainStreamCaption: "main stream, child work folded",
  childStreamCaption: "child stream",
  tools: "tools",
  externalInput: "External · Input",
  noOpeningLine: "no opening line",
  delegatedWork: "delegated work",
  showWork: "show what the agent did",
  hideWork: "hide what the agent did",
  quiet: "quiet",
  mainAgent: "Main agent",
  childAgent: "Child agent",
  agent: "Agent",
  response: "Response",
  streamOutput: "Stream output",
  clientMadeMessage: "Client-made message · not from the provider",
  contextPutIn: "Context put in",
  agentCallToChild: "Agent call",
  notificationFromChild: "Runtime notification",
  launchAcknowledged: "Launch acknowledged",
  modelCall: "Model call",
  reasoning: "Reasoning",
  toolWord: "Tool",
  callWord: "call",
  turn: "turn",
  input: "input",
  result: "result",
  failed: "failed",
  toReturn: "to return",
  openStream: "Open",
  independentStream: "Independent stream context. Its messages are not merged into the parent.",
  openedFrom: "Opened from",
  noOpenerRecorded: "No relation records what opened it.",
  backToOpener: "← Back to the step that opened this",
  noTalksInStream: "No talks in this stream.",
  nothingFolded: "Nothing of this conversation could be folded, so there is nothing to read. The problems listed above say why.",
  outsideAnyTalk: "Outside any talk",
  outsideAnyTalkNote: "Steps the fold could not place under a talk — a child’s output whose stream opened no talk, for instance. The document holds them so nothing is lost.",
  childStreamsPick: "child streams…",
  childStreamSingular: "child stream",
  childStreamPlural: "child streams",
  backToStream: "Back to",
  showThisTalk: "Show this talk in the flow timeline",
  showThisAnswer: "Show this talk’s answer in the flow timeline",
  locateInTimeline: "Locate in the flow timeline",
  flowTimeline: "Flow timeline",
  timelineHelp: "What is the flow timeline?",
  parentTimeline: "← Parent timeline",
  fadeUnrelated: "Fade unrelated",
  fadeUnrelatedTitle: "Fade the steps that the selected one does not touch",
  zoom: "Zoom",
  centerSelected: "Center selected",
  centerSelectedTitle: "Scroll the timeline to the selected step",
  events: "events",
  nested: "nested",
  laneInput: "External input",
  laneResponses: "Responses",
  laneContext: "Context put in",
  laneModel: "Model calls",
  laneTools: "Tools",
  laneAgents: "Agent activity",
  laneNotices: "Runtime notices",
  laneNested: "Nested streams",
  legendInput: "External input",
  legendResponse: "Agent response",
  legendModel: "Model call",
  legendTool: "Tool",
  legendAgent: "Agent / nested stream",
  legendContext: "Context / annotation",
  legendOwns: "Emitted by (ownership)",
  legendExact: "Exact join",
  legendInferred: "Strong inference",
  agentsCreated: "agents created",
  poolTitle: "This call started {n} child agents at nearly the same moment. Click, then pick one in the inspector.",
  reportedHere: "It reported finishing in this talk.",
  clickToSeeOpener: "Click to see what opened it.",
  helpWhat: "what it shows",
  helpWhatText: "One talk, on a time axis. Each row is a lane: the messages, the model calls, the tools it ran, its agent activity, and the nested streams it started. The line above the lanes is the whole conversation, one cell per segment.",
  helpHeading: "the heading",
  helpHeadingText: "Which stream the lanes belong to, how many steps are in this talk, and how many nested streams it touches.",
  helpAxis: "the axis",
  helpAxisText: "Busy stretches take the width. A long pause is cut to a short gap marked with how long it was, so a talk with an hour of waiting still reads.",
  helpLinks: "links",
  helpLinksText: "The lines between lanes, drawn for the step you select. A solid line is an exact join, a dashed one was inferred, and a faint curve is ownership: the model call that produced a step.",
  helpFade: "fade unrelated",
  helpFadeText: "After you click a step, fade everything it does not touch.",
  inspector: "Inspector",
  details: "Details",
  evidence: "Evidence",
  selectAStep: "Select a step.",
  nestedStream: "nested stream",
  stream: "Stream",
  role: "Role",
  openedBy: "Opened by",
  reportedHereBy: "Reported here by",
  note: "Note",
  reportedNote: "this talk hears the ending. The call that started it is in an earlier talk, because a launch and its completion land in different turns.",
  joinQuality: "Join quality",
  openedAt: "Opened at",
  itsAnswer: "Its answer",
  itsAnswerNote: "not in this stream. A child’s output belongs to the child, so the parent holds a boundary that refers to it, never a copy. Dive in to read it.",
  opensInTurn: "Opens in turn",
  opensNothing: "nothing — it opened no stream of its own",
  andMore: "and {n} more",
  poolWarning: "this call started {n} streams",
  poolWarningText: "A workflow call starts a run, and the run starts a pool of agents. They are all its children, not competing guesses at one child.",
  diveIn: "Dive in →",
  diveHint: "Enter to dive in · Escape to clear",
  nodeKind: "Node kind",
  lane: "Lane",
  segment: "Segment",
  run: "Run",
  directParent: "Direct parent",
  observedAt: "Observed at",
  requestToResult: "Request to result",
  duration: "Duration",
  durationUnavailable: "unavailable — the runtime does not report how long this ran",
  requestToResultWhat: 'What "request to result" is',
  requestToResultText: "The time between the record carrying the request and the record carrying the result. An exact identifier ties those two records together, so the interval belongs to this call. It is not how long the tool ran, and the runtime does not report that.",
  name: "Name",
  failedField: "Failed",
  yes: "yes",
  no: "no",
  contentState: "Content state",
  contentBytes: "Content bytes",
  tokens: "Tokens",
  tokensText: "in {in} · out {out} · cache read {cacheRead} · cache write {cacheWrite}",
  contentUnavailable: "Content {state}",
  contentUnavailableText: "The source did not supply readable content for this step. It is reported as it was found, not filled in.",
  oneChildAgent: "1 child agent",
  childAgents: "{n} child agents",
  openRelations: "open the Relations tab →",
  backToParentStream: "← Back to parent stream",
  noRelation: "No relation touches this step.",
  agentsCreatedByCall: "{n} agents created by this call",
  agentsCreatedText: "A workflow call starts a run, and the run starts a pool of agents. Every one of them is a child of this call, not a competing guess at one child. Pick the one to read.",
  outgoing: "outgoing",
  incoming: "incoming",
  joinedOn: "joined on",
  namedFromJournal: "named from its run journal",
  otherEndOutside: "the other end is outside this stream",
  derivedByAssembly: "This step was derived by assembly. It cites no landed record.",
  landedPositions: "landed positions",
  request: "request",
  part: "part",
  record: "record",
  clippedText: "the text as the document carries it",
  fullTextNote: "clipped: {shown} of {total} bytes",
  loadFullRecord: "Load the landed record",
  loadingRecord: "reading…",
  recordFailed: "The record could not be read.",
  theLandedRecord: "the landed record",
  dropped: "dropped",
  flags: "flags",
  unavailable: "unavailable",
  vocabulary: "vocabulary",
  modelOwnWord: "the model’s own word",
  aszTerm: "asz term",
  runtimeWord: "runtime word",
  runtimeNoWord: "none — this is derived here, the runtime has no word for it",
  whereToLook: "where to look",
  nowhereInSource: "nowhere in the source; it is produced by assembly",
  readCarefully: "read it carefully",
  dialect: "dialect",
  landedRecordField: "a field of the landed record",
  aszField: "asz field",
  whatItIs: "what it is",
  notApplicable: "not applicable — this describes the landed record, not the source",
  close: "Close",
  whatDoesMean: "What does {key} mean?",
  selected: "Selected {what}",
  selectedNestedStream: "Selected nested stream {name}, opened by {kind}. Press Enter to dive in, Escape to clear.",
  selectionCleared: "Selection cleared."
};
function fill(template, vars) {
  return template.replace(/\{(\w+)\}/g, (_, k) => k in vars ? String(vars[k]) : `{${k}}`);
}
function streamName(ctx, name) {
  const st = ctx.model.streamByName.get(name);
  if (!st) return name;
  if (st.role === "main") return ctx.s.mainAgent;
  return st.label || `${ctx.s.childStreamSingular} ${st.name.slice(0, 6)}`;
}
function drawStatus(ctx) {
  const { model: m, s, f, state } = ctx;
  const sum = m.doc.summary;
  const span = sum.to && sum.from ? f.duration(sum.to - sum.from) : "—";
  const verified = m.doc.rounds.filter((r) => r.verified).length;
  const integrity = sum.state === "verified" ? s.integrityVerified : sum.state === "mismatch" ? s.integrityMismatch : s.integrityIncomplete;
  ctx.q(".acv-status").innerHTML = `<span class="acv-badge acv-integrity is-${esc(sum.state)}" title="${esc(`${verified}/${m.doc.rounds.length} ${s.roundsVerified} · ${m.doc.files.length} ${s.filesListed}`)}"><span class="acv-dot"></span>${esc(integrity)}${sum.problems.length ? ` · ${sum.problems.length} ${esc(s.problems)}` : ""}</span><span class="acv-badge"><span class="acv-dot"></span>${esc(s.round)} ${f.number(m.doc.head.round)}</span><span><strong>${f.number(m.doc.segments.length)}</strong> ${esc(s.segments)}</span><span><strong>${f.number(m.doc.streams.length)}</strong> ${esc(s.streams)}</span><span><strong>${f.number(m.talks.length)}</strong> ${esc(s.talks)}</span><span>${esc(span)} ${esc(s.span)}</span>` + (sum.unresolved ? `<span class="acv-warn">${f.number(sum.unresolved)} ${esc(s.unresolved)}</span>` : "") + `<button type="button" class="acv-overview-toggle" aria-expanded="${state.overviewOpen}" aria-controls="acv-overview">${esc(s.overview)} <span class="acv-chevron" aria-hidden="true">⌄</span></button>`;
  ctx.q(".acv-overview-toggle").addEventListener("click", () => setOverviewOpen(ctx, !state.overviewOpen));
  if (sum.problems.length) {
    ctx.q(".acv-integrity").addEventListener("click", () => setProblemsOpen(ctx, ctx.q(".acv-problems").hidden));
    ctx.q(".acv-problems-close").addEventListener("click", () => setProblemsOpen(ctx, false));
  }
  ctx.q(".acv-problems-title").textContent = `${f.number(sum.problems.length)} ${s.problems}`;
  ctx.q(".acv-problems-body").innerHTML = sum.problems.length ? `<ul>${sum.problems.map((p) => `<li>${esc(p)}</li>`).join("")}</ul>` : "";
  setProblemsOpen(ctx, sum.steps === 0 && sum.problems.length > 0);
}
function setProblemsOpen(ctx, open) {
  const box = ctx.q(".acv-problems");
  box.hidden = !open;
  if (open && ctx.state.overviewOpen) setOverviewOpen(ctx, false);
}
function setOverviewOpen(ctx, open) {
  ctx.state.overviewOpen = open;
  ctx.q(".acv-overview").hidden = !open;
  if (open) ctx.q(".acv-problems").hidden = true;
  ctx.q(".acv-overview-toggle").setAttribute("aria-expanded", String(open));
  if (open) ctx.q(".acv-talk-filter").focus();
}
function drawOverview(ctx) {
  var _a, _b;
  const { model: m, s, f } = ctx;
  const sum = m.doc.summary;
  const child = m.doc.streams.filter((x) => x.role !== "main").length;
  const runs = ((_a = sum.kinds) == null ? void 0 : _a.run) ?? 0;
  const quality = Object.entries(sum.quality ?? {}).sort((a, b) => b[1] - a[1]).map(([k, n]) => `${f.number(n)} ${k.replace(/_/g, " ")}`).join(" · ");
  const idle = ((_b = /idle=([^\s,]+)/.exec(m.doc.policy)) == null ? void 0 : _b[1]) ?? m.doc.policy;
  ctx.q(".acv-summary").innerHTML = `
    <div class="acv-cell acv-cell-primary"><div class="acv-kicker">${esc(s.session)}</div><strong>${esc(m.doc.sessions.join(", ") || m.doc.conversation)}</strong></div>
    <div class="acv-cell"><div class="acv-kicker">${esc(s.steps)}</div><strong>${f.number(sum.steps)}</strong>
      <div class="acv-cell-sub">${f.number(m.talks.length)} ${esc(s.talks)} · ${f.number(runs)} ${esc(s.runs)}</div></div>
    <div class="acv-cell"><div class="acv-kicker">${esc(s.childStreams)}</div><strong>${f.number(child)}</strong>
      <div class="acv-cell-sub">${esc(s.childStreamsNote)}</div></div>
    <div class="acv-cell"><div class="acv-kicker">${esc(s.relations)}</div><strong>${f.number(m.doc.relations.length)}</strong>
      <div class="acv-cell-sub">${esc(quality)}</div></div>
    <div class="acv-cell"><div class="acv-kicker">${esc(s.segmentList)}</div><strong>${f.number(m.doc.segments.length)}</strong>
      <div class="acv-cell-sub">${esc(s.activityWindows)}, ${esc(s.idle)} ${esc(idle)}</div></div>`;
}
function drawTalkList(ctx) {
  const { model: m, s, f, state } = ctx;
  const filter = (ctx.q(".acv-talk-filter").value ?? "").toLowerCase();
  const rows = m.talks.filter(
    (t) => !t.child && (!filter || t.label.toLowerCase().includes(filter) || t.reply.toLowerCase().includes(filter))
  );
  const list = ctx.q(".acv-talk-list");
  list.innerHTML = rows.map(
    (t) => `<button type="button" class="acv-pick${state.talk && t.id === state.talk.id ? " on" : ""}" data-talk-pick="${esc(t.id)}">
      <b>${esc(t.label || s.noOpeningLine)}</b>
      ${t.reply ? `<span class="acv-pick-reply">${esc(t.reply)}</span>` : ""}
      <small>${esc(f.time(t.from))} · ${f.number(t.steps)} ${esc(s.steps.toLowerCase())} · ${t.runs}r${t.tools ? ` · ${t.tools} ${esc(s.tools)}` : ""}</small></button>`
  ).join("") || `<div class="acv-empty">${esc(s.noTalkMatches)}</div>`;
}
function drawStreamTabs(ctx) {
  const { model: m, s, f, state } = ctx;
  const t = state.talk;
  const own = state.stream ? m.streamByName.get(state.stream) : void 0;
  const list = [];
  if (own && own.role !== "main") {
    const up = m.openerOf(own.name);
    if (up) {
      const parent = m.streamByName.get(up.step.stream);
      if (parent) list.push({ name: parent.name, role: parent.role, label: parent.label, steps: parent.steps, isParent: true, opener: { step: up.step.id, talk: up.talk } });
    }
  }
  if (own) list.push({ name: own.name, role: own.role, label: own.label, steps: own.steps });
  if (state.stream) {
    for (const fo of m.foldersFor(state.stream)) {
      if (!list.some((x) => x.name === fo.stream.name)) list.push({ name: fo.stream.name, role: fo.stream.role, label: fo.stream.label, steps: fo.stream.steps });
    }
  }
  const tabs = list.filter((x) => x.role === "main" || x.name === state.stream);
  const rest = list.filter((x) => !tabs.includes(x));
  let html = tabs.map(
    (x) => x.isParent ? `<button type="button" class="acv-stream-btn up" data-up-stream="${esc(x.name)}" data-up-step="${esc(x.opener.step)}" data-up-talk="${esc(x.opener.talk ?? "")}" title="${esc(`${s.backToStream} ${x.name}`)}">↰ ${esc(streamName(ctx, x.name))}</button>` : `<button type="button" role="tab" class="acv-stream-btn${state.stream === x.name ? " active" : ""}" aria-selected="${state.stream === x.name}" data-tab-stream="${esc(x.name)}" title="${esc(x.name)}">${x.role === "main" ? "" : "↳ "}${esc(streamName(ctx, x.name))}</button>`
  ).join("");
  if (rest.length) {
    html += `<select class="acv-stream-btn acv-child-pick" title="${esc(s.childStreams)}">
      <option value="">↳ ${rest.length} ${esc(rest.length === 1 ? s.childStreamSingular : s.childStreamPlural)}…</option>
      ${rest.map((x) => `<option value="${esc(x.name)}">${esc(x.label || `${s.childStreamSingular} ${x.name.slice(0, 6)}`)} · ${f.number(x.steps)} ${esc(s.steps.toLowerCase())}</option>`).join("")}</select>`;
  }
  const host = ctx.q(".acv-stream-tabs");
  host.innerHTML = html;
  host.querySelectorAll("[data-tab-stream]").forEach((b) => b.onclick = () => ctx.switchStream(b.dataset.tabStream));
  host.querySelectorAll("[data-up-stream]").forEach(
    (b) => b.onclick = () => ctx.goToOpener(b.dataset.upStream, b.dataset.upStep, b.dataset.upTalk || null)
  );
  const pick = host.querySelector(".acv-child-pick");
  if (pick) {
    pick.onchange = () => {
      var _a;
      if (!pick.value) return;
      state.navStack.push({ stream: state.stream, sel: state.sel, talk: ((_a = state.talk) == null ? void 0 : _a.id) ?? null });
      ctx.switchStream(pick.value);
    };
  }
  const bits = [];
  if (t) {
    bits.push(own && own.role === "main" ? s.mainStreamCaption : s.childStreamCaption);
    if (t.steps) bits.push(`${f.number(t.steps)} ${s.steps.toLowerCase()}`);
    if (t.runs) bits.push(`${t.runs} ${s.runs}`);
    if (t.tools) bits.push(`${f.number(t.tools)} ${s.tools}`);
    if (t.to > t.from) bits.push(f.duration(t.to - t.from));
  }
  const caption = ctx.q(".acv-talk-caption");
  caption.textContent = bits.join(" · ");
  caption.title = t ? t.id : "";
}
function field(dt, dd, mono = false) {
  return `<dt>${esc(dt)}</dt><dd class="${mono ? "mono" : ""}">${dd}</dd>`;
}
function drawInspector(ctx) {
  const { s, f, model: m, state } = ctx;
  if (state.folder) {
    drawFolderPanel(ctx);
    return;
  }
  const e = m.step(state.sel);
  const relTab = ctx.q('[data-tab="relations"]');
  const hasRels = !!(e && e.edges.length);
  relTab.hidden = !hasRels;
  if (!hasRels && state.tab === "relations") {
    ctx.showTab("details");
    return;
  }
  const title = ctx.q(".acv-inspector-title");
  const meta = ctx.q(".acv-inspector-meta");
  const body = ctx.q(".acv-inspector-body");
  if (!e) {
    title.textContent = "—";
    meta.textContent = "";
    body.innerHTML = `<div class="acv-empty">${esc(s.selectAStep)}</div>`;
    return;
  }
  title.textContent = e.name ? `${e.kind} · ${e.name}` : kindTitle(e.kind, s);
  meta.textContent = `${e.stream.slice(0, 12)} · ${f.time(e.at)}${e.bytes ? ` · ${f.number(e.bytes)} B` : ""}`;
  if (state.tab === "details") drawDetails(ctx, body, e);
  else if (state.tab === "relations") drawRelations(ctx, body, e);
  else void drawEvidence(ctx, body, e);
}
function drawFolderPanel(ctx) {
  var _a;
  const { s, f, model: m, state } = ctx;
  const folder = m.foldersFor(state.stream).find((x) => x.stream.name === state.folder);
  if (!folder) {
    state.folder = null;
    drawInspector(ctx);
    return;
  }
  const st = folder.stream;
  const opener = folder.from;
  ctx.q(".acv-inspector-title").textContent = st.label || `${s.childStreamSingular} ${st.name.slice(0, 6)}`;
  ctx.q(".acv-inspector-meta").textContent = `${s.nestedStream} · ${f.number(st.steps)} ${s.steps.toLowerCase()} · ${folder.quality}`;
  const many = folder.candidates > 1;
  const own = m.openedBy(st.name);
  const body = ctx.q(".acv-inspector-body");
  body.innerHTML = `
    <div class="acv-path">${esc(s.stream.toLowerCase())} ${esc(st.name)} › ${esc(s.openedFrom.toLowerCase())} ${esc(opener.kind)} · ${esc(state.stream ?? "")}</div>
    <dl class="acv-definition">
      ${field(s.stream, esc(st.name), true)}
      ${field(s.role, esc(st.role))}
      ${field(s.steps, f.number(st.steps))}
      ${field(
    folder.back ? s.reportedHereBy : s.openedBy,
    `<button type="button" class="acv-linkish" data-goto="${esc(opener.id)}">${esc(opener.kind)}${opener.name ? ` · ${esc(opener.name)}` : ""}</button>`
  )}
      ${folder.back ? field(s.note, `<span class="acv-faint">${esc(s.reportedNote)}</span>`) : ""}
      ${field(s.joinQuality, esc(folder.quality))}
      ${field(s.openedAt, opener.at ? esc(f.time(opener.at)) : esc(s.unavailable))}
      ${field(s.itsAnswer, `<span class="acv-faint">${esc(s.itsAnswerNote)}</span>`)}
      ${field(
    s.opensInTurn,
    own.length ? own.slice(0, 6).map((x) => `<button type="button" class="acv-linkish" data-dive>${esc(x.label || `${s.childStreamSingular} ${x.name.slice(0, 6)}`)}</button>`).join("<br>") + (own.length > 6 ? `<br><span class="acv-faint">${esc(fill(s.andMore, { n: own.length - 6 }))}</span>` : "") : `<span class="acv-faint">${esc(s.opensNothing)}</span>`
  )}
    </dl>
    ${many ? `<div class="acv-warning"><strong>${esc(fill(s.poolWarning, { n: folder.candidates }))}</strong><br>${esc(s.poolWarningText)}</div>` : ""}
    <div class="acv-dive">
      <button type="button" class="acv-btn primary" data-dive-in>${esc(s.diveIn)}</button>
      <span class="acv-hint">${esc(s.diveHint)}</span>
    </div>`;
  body.querySelector("[data-dive-in]").onclick = () => ctx.diveIn();
  body.querySelectorAll("[data-dive]").forEach((b) => b.onclick = () => ctx.diveIn());
  (_a = body.querySelector("[data-goto]")) == null ? void 0 : _a.addEventListener("click", (ev) => ctx.select(ev.currentTarget.dataset.goto, true));
}
function drawDetails(ctx, body, e) {
  var _a, _b;
  const { s, f, model: m, state } = ctx;
  const st = m.streamByName.get(e.stream);
  const seg = state.talk ? m.segmentById.get(state.talk.segment) : void 0;
  const path = [`${m.doc.conversation.slice(0, 8)}`];
  if (seg) path.push(`${s.segment.toLowerCase()} ${seg.id.replace(/^segment\//, "")}`);
  path.push(`${s.stream.toLowerCase()} ${e.stream.slice(0, 10)}`);
  if (e.talk) path.push(`${s.talk.toLowerCase()} ${e.talk.replace(/^talk\//, "").slice(0, 14)}`);
  if (e.run) path.push(`${s.run.toLowerCase()} ${e.run.replace(/^run\//, "").slice(0, 14)}`);
  path.push(`${e.kind} ${e.id.slice(0, 20)}`);
  let html = `<div class="acv-path">${path.map(esc).join(" › ")}</div><dl class="acv-definition">`;
  html += field(s.nodeKind, esc(e.kind));
  html += field(s.lane, esc(e.track));
  html += field(s.stream, `${esc(e.stream)}${st ? ` · ${esc(st.role)}` : ""}`, true);
  html += field(s.segment, seg ? esc(seg.id.replace(/^segment\//, "")) : "—");
  html += field(s.talk, e.talk ? esc(e.talk.replace(/^talk\//, "")) : "—", true);
  html += field(s.run, e.run ? esc(e.run.replace(/^run\//, "")) : "—", true);
  html += field(s.directParent, e.parent ? esc(e.parent) : "—", true);
  html += field(s.observedAt, e.at ? esc(f.dateTime(e.at)) : esc(s.unavailable));
  if (e.reqToRes != null) {
    html += field(s.requestToResult, `${esc(f.duration(e.reqToRes))} <span class="acv-faint">· ${esc(e.reqToResJoin ?? "")}</span>`);
  }
  html += field(
    s.duration,
    e.durationMs ? `${esc(f.duration(e.durationMs))}${e.durationHow ? ` <span class="acv-faint">· ${esc(e.durationHow)}</span>` : ""}` : `<span class="acv-faint">${esc(s.durationUnavailable)}</span>`
  );
  if (e.reqToRes != null) {
    html += `<dt></dt><dd><div class="acv-warning" style="margin:0"><strong>${esc(s.requestToResultWhat)}</strong><br>${esc(s.requestToResultText)}</div></dd>`;
  }
  if (e.name) html += field(s.name, esc(e.name));
  if (e.failed !== void 0) html += field(s.failedField, e.failed ? esc(s.yes) : esc(s.no));
  if (e.state) html += field(s.contentState, esc(e.state));
  if (e.bytes) html += field(s.contentBytes, f.number(e.bytes));
  if (e.usage) {
    html += field(
      s.tokens,
      esc(
        fill(s.tokensText, {
          in: f.number(e.usage.in ?? 0),
          out: f.number(e.usage.out ?? 0),
          cacheRead: f.number(e.usage.cache_read ?? 0),
          cacheWrite: f.number(e.usage.cache_write ?? 0)
        })
      )
    );
  }
  html += `</dl>`;
  if (e.text) html += `<div class="acv-block">${esc(e.text)}</div>`;
  if (e.result) {
    html += `<div class="acv-kicker" style="margin-top:14px">${esc(s.result)}${e.failed ? ` · ${esc(s.failed)}` : ""}${e.resultBytes ? ` · ${f.number(e.resultBytes)} B` : ""}</div><div class="acv-block">${esc(e.result)}</div>`;
  } else if (e.state && e.state !== "available") {
    html += `<div class="acv-warning"><strong>${esc(fill(s.contentUnavailable, { state: e.state }))}</strong><br>${esc(s.contentUnavailableText)}</div>`;
  }
  const folders = e.edges.filter((g) => g.type === "starts" && g.dir === "out");
  if (folders.length) {
    html += `<button type="button" class="acv-linkish" data-to-relations style="margin-top:14px">${esc(
      folders.length === 1 ? s.oneChildAgent : fill(s.childAgents, { n: folders.length })
    )} — ${esc(s.openRelations)}</button>`;
  }
  if (state.navStack.length) html += `<button type="button" class="acv-btn" style="margin-top:12px" data-back-parent>${esc(s.backToParentStream)}</button>`;
  body.innerHTML = html;
  (_a = body.querySelector("[data-to-relations]")) == null ? void 0 : _a.addEventListener("click", () => ctx.showTab("relations"));
  (_b = body.querySelector("[data-back-parent]")) == null ? void 0 : _b.addEventListener("click", () => ctx.goBack());
}
function drawRelations(ctx, body, e) {
  const { s, model: m, state } = ctx;
  const rels = e.edges;
  if (!rels.length) {
    body.innerHTML = `<div class="acv-empty">${esc(s.noRelation)}</div>`;
    return;
  }
  const children = rels.filter((r) => m.streamById.has(r.other));
  let html = "";
  if (children.length > 1) {
    html += `<div class="acv-warning"><strong>${esc(fill(s.agentsCreatedByCall, { n: children.length }))}</strong><br>${esc(s.agentsCreatedText)}</div>`;
  }
  html += `<div class="acv-relation-list">${rels.map((r) => {
    const cls = r.quality === "exact_unique" ? "" : r.quality === "strong_inference" ? " inferred" : " weak";
    const st = m.streamById.get(r.other);
    if (st) {
      const about = !st.label && st.talk ? m.talkById.get(st.talk) : void 0;
      return `<div class="acv-relation-card${cls}">
        <button type="button" data-open-child="${esc(st.name)}"><strong>${esc(r.type)} · ${esc(s.childStreamSingular)} ${esc(st.name.slice(0, 6))}</strong><br>${st.label ? `${esc(st.label)} →` : `${esc(s.openStream)} →`}</button>
        ${(about == null ? void 0 : about.label) ? `<small class="acv-child-about">${esc(about.label.slice(0, 200))}</small>` : ""}
        <small>${esc(r.quality)}${r.via ? ` — ${esc(s.joinedOn)} ${esc(r.via)}` : ""}${st.named_by === "journal" ? ` — ${esc(s.namedFromJournal)}` : ""}</small></div>`;
    }
    const known = !!m.step(r.other);
    return `<div class="acv-relation-card${cls}">
        <button type="button" ${known ? `data-rel="${esc(r.other)}"` : "disabled"}><strong>${esc(r.dir === "out" ? s.outgoing : s.incoming)} · ${esc(r.type)}</strong><br>${esc(r.other)}</button>
        <small>${esc(r.quality)}${r.via ? ` — ${esc(s.joinedOn)} ${esc(r.via)}` : ""}${known ? "" : `<br>${esc(s.otherEndOutside)}`}</small></div>`;
  }).join("")}</div>`;
  body.innerHTML = html;
  body.querySelectorAll("[data-rel]").forEach((b) => b.onclick = () => ctx.select(b.dataset.rel, true));
  body.querySelectorAll("[data-open-child]").forEach(
    (b) => b.onclick = () => {
      var _a;
      state.navStack.push({ stream: state.stream, sel: state.sel, talk: ((_a = state.talk) == null ? void 0 : _a.id) ?? null });
      ctx.switchStream(b.dataset.openChild);
    }
  );
}
async function drawEvidence(ctx, body, e) {
  var _a;
  const { s, f, state } = ctx;
  const refs = (e.ref ? [e.ref] : []).concat(e.refs ?? []);
  const seen = /* @__PURE__ */ new Set();
  const list = refs.filter((r) => {
    const k = `${r.seq}/${r.row}/${r.block ?? ""}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
  if (!list.length) {
    body.innerHTML = `<div class="acv-empty">${esc(s.derivedByAssembly)}</div>`;
    return;
  }
  const pick = (state.rawRef && list.find((r) => r.seq === state.rawRef.seq && r.row === state.rawRef.row)) ?? list[0];
  const role = (i) => e.kind === "tool" || e.kind === "agent.call" ? i === 0 ? s.request : s.result : list.length > 1 ? `${s.part} ${i + 1}` : s.record;
  const shown = e.text ? new TextEncoder().encode(e.text).length : 0;
  const clipped = e.bytes && shown && e.bytes > shown;
  body.innerHTML = `
    <div class="acv-kicker">${esc(s.landedPositions)}</div>
    <div class="acv-ref-row">${list.map(
    (r, i) => `<button type="button" class="acv-ref-chip${r === pick ? " on" : ""}" data-ref="${r.seq}/${r.row}">
        <b>${esc(role(i))}</b><span>seq ${r.seq} · row ${r.row}${r.block != null ? ` · block ${r.block}` : ""}</span></button>`
  ).join("")}</div>
    ${e.text ? `<div class="acv-kicker" style="margin-top:12px">${esc(s.clippedText)}${clipped ? ` · ${esc(fill(s.fullTextNote, { shown: f.number(shown), total: f.number(e.bytes) }))}` : ""}</div>
      <div class="acv-block">${esc(e.text)}</div>` : ""}
    ${((_a = e.flags) == null ? void 0 : _a.length) ? `<div class="acv-kicker" style="margin-top:12px">${esc(s.flags)}</div><div class="acv-provenance">${e.flags.map((x) => `<span class="acv-source-badge">${esc(x)}</span>`).join("")}</div>` : ""}
    ${(e.dropped ?? []).map((d) => `<div class="acv-warning">${esc(s.dropped)} ${esc(d.what)} · ${f.number(d.bytes)} B<br>${esc(d.why ?? "")}</div>`).join("")}
    <div class="acv-explain-box"></div>
    <div class="acv-record-box">${ctx.loadRecord ? `<button type="button" class="acv-btn" data-load-record>${esc(s.loadFullRecord)}</button>` : ""}</div>`;
  body.querySelectorAll("[data-ref]").forEach(
    (b) => b.onclick = () => {
      const [seq, row] = b.dataset.ref.split("/").map(Number);
      state.rawRef = { seq, row };
      state.explain = null;
      ctx.drawInspector();
    }
  );
  const loadBtn = body.querySelector("[data-load-record]");
  if (loadBtn && ctx.loadRecord) {
    loadBtn.onclick = async () => {
      const box = body.querySelector(".acv-record-box");
      box.innerHTML = `<div class="acv-empty">${esc(s.loadingRecord)}</div>`;
      let rec;
      try {
        rec = await ctx.loadRecord(pick);
      } catch {
        box.innerHTML = `<div class="acv-warning">${esc(s.recordFailed)}</div>`;
        return;
      }
      if (!box.isConnected) return;
      box.innerHTML = `<div class="acv-kicker" style="margin-top:14px">${esc(s.theLandedRecord)}</div><pre class="acv-raw">${renderJSON(ctx, rec, 0)}</pre>`;
      box.querySelectorAll("[data-term]").forEach(
        (b) => b.onclick = (ev) => {
          ev.stopPropagation();
          state.explain = state.explain === b.dataset.term ? null : b.dataset.term;
          box.querySelectorAll("[data-term]").forEach((x) => x.classList.toggle("on", x.dataset.term === state.explain));
          drawExplain(ctx, body);
        }
      );
    };
  }
  drawExplain(ctx, body);
}
function explainable(ctx, key) {
  var _a, _b;
  const g = ctx.glossary;
  if (!g) return null;
  if ((_a = g.terms) == null ? void 0 : _a[key]) return "term";
  if ((_b = g.fields) == null ? void 0 : _b[key]) return "field";
  return null;
}
function renderJSON(ctx, v, depth, key) {
  const pad = "  ".repeat(depth);
  const label = key == null ? "" : explainable(ctx, key) ? `<span class="acv-jkey">"${esc(key)}"</span><button type="button" class="acv-q" data-term="${esc(key)}" aria-label="${esc(fill(ctx.s.whatDoesMean, { key }))}" title="${esc(fill(ctx.s.whatDoesMean, { key }))}">?</button>: ` : `<span class="acv-jkey plain">"${esc(key)}"</span>: `;
  if (v === null) return `${pad}${label}<span class="acv-jnull">null</span>
`;
  if (Array.isArray(v)) {
    if (!v.length) return `${pad}${label}[]
`;
    return `${pad}${label}[
${v.map((x) => renderJSON(ctx, x, depth + 1)).join("")}${pad}]
`;
  }
  if (typeof v === "object") {
    const ks = Object.keys(v);
    if (!ks.length) return `${pad}${label}{}
`;
    return `${pad}${label}{
${ks.map((k) => renderJSON(ctx, v[k], depth + 1, k)).join("")}${pad}}
`;
  }
  const cls = typeof v === "number" ? "acv-jnum" : typeof v === "boolean" ? "acv-jbool" : "acv-jstr";
  const text = typeof v === "string" ? `"${v.length > 300 ? `${v.slice(0, 300)}…` : v}"` : String(v);
  return `${pad}${label}<span class="${cls}">${esc(text)}</span>
`;
}
function drawExplain(ctx, body) {
  var _a;
  const { s, state, glossary: g } = ctx;
  const host = body.querySelector(".acv-explain-box");
  if (!host) return;
  const key = state.explain;
  if (!key || !g) {
    host.innerHTML = "";
    return;
  }
  const t = (_a = g.terms) == null ? void 0 : _a[key];
  const rows = [];
  if (t) {
    rows.push([s.vocabulary, `${esc(s.modelOwnWord)} · <span class="mono">${esc(s.aszTerm)}</span>`]);
    rows.push([s.aszTerm, `<span class="mono">${esc(key)}</span>`]);
    rows.push([s.runtimeWord, t.native ? `<span class="mono">${esc(t.native)}</span>` : `<span class="acv-faint">${esc(s.runtimeNoWord)}</span>`]);
    rows.push([s.whereToLook, t.where ? esc(t.where) : `<span class="acv-faint">${esc(s.nowhereInSource)}</span>`]);
    if (t.note) rows.push([s.readCarefully, esc(t.note)]);
    rows.push([s.dialect, `<span class="mono">${esc(g.dialect)}</span>`]);
  } else {
    rows.push([s.vocabulary, `${esc(s.landedRecordField)} · <span class="mono">.sd</span>`]);
    rows.push([s.aszField, `<span class="mono">${esc(key)}</span>`]);
    rows.push([s.whatItIs, esc(g.fields[key] ?? "")]);
    rows.push([s.runtimeWord, `<span class="acv-faint">${esc(s.notApplicable)}</span>`]);
  }
  host.innerHTML = `<div class="acv-explain">
    <div class="acv-explain-head"><span class="acv-explain-title">${esc(key)}</span>
      <button type="button" class="acv-explain-close" aria-label="${esc(s.close)}">×</button></div>
    <dl class="acv-explain-rows">${rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join("")}</dl></div>`;
  host.querySelector(".acv-explain-close").onclick = () => {
    state.explain = null;
    drawExplain(ctx, body);
  };
}
const PANELS = [
  {
    key: "acv.inspector",
    cssVar: "--acv-inspector",
    handle: ".acv-split-inspector",
    axis: "x",
    min: 260,
    max: () => Math.max(280, (globalThis.innerWidth || 1280) - 420),
    fallback: 350,
    measure: (root, e) => root.querySelector(".acv-workbench").getBoundingClientRect().right - e.clientX,
    current: (root) => root.querySelector(".acv-inspector").getBoundingClientRect().width
  },
  {
    key: "acv.dock",
    cssVar: "--acv-dock",
    handle: ".acv-split-dock",
    axis: "y",
    min: 150,
    max: () => Math.max(160, (globalThis.innerHeight || 800) - 260),
    fallback: null,
    measure: (root, e) => root.querySelector(".acv-workbench").getBoundingClientRect().bottom - e.clientY,
    current: (root) => root.querySelector(".acv-dock").getBoundingClientRect().height
  }
];
function read(key) {
  var _a;
  try {
    return Number(((_a = globalThis.localStorage) == null ? void 0 : _a.getItem(key)) ?? 0);
  } catch {
    return 0;
  }
}
function write(key, v) {
  var _a;
  try {
    (_a = globalThis.localStorage) == null ? void 0 : _a.setItem(key, String(v));
  } catch {
  }
}
function setupPanels(ctx) {
  var _a;
  const { root } = ctx;
  const set = (p, px) => {
    const v = Math.round(Math.min(p.max(), Math.max(p.min, px)));
    root.style.setProperty(p.cssVar, `${v}px`);
    write(p.key, v);
    return v;
  };
  const onResize = () => {
    for (const p of PANELS) set(p, p.current(root));
  };
  for (const p of PANELS) {
    const stored = read(p.key);
    if (stored > 0) set(p, stored);
    else if (p.fallback) set(p, p.fallback);
    const bar = root.querySelector(p.handle);
    if (!bar) continue;
    bar.addEventListener("pointerdown", (e) => {
      e.preventDefault();
      bar.setPointerCapture(e.pointerId);
      bar.classList.add("dragging");
      root.classList.add("acv-resizing");
      const move = (ev) => {
        set(p, p.measure(root, ev));
      };
      const up = (ev) => {
        bar.releasePointerCapture(ev.pointerId);
        bar.classList.remove("dragging");
        root.classList.remove("acv-resizing");
        bar.removeEventListener("pointermove", move);
        bar.removeEventListener("pointerup", up);
        ctx.drawTimeline();
      };
      bar.addEventListener("pointermove", move);
      bar.addEventListener("pointerup", up);
    });
    bar.addEventListener("keydown", (e) => {
      const step = e.shiftKey ? 48 : 12;
      const grow = p.axis === "x" ? "ArrowLeft" : "ArrowUp";
      const shrink = p.axis === "x" ? "ArrowRight" : "ArrowDown";
      if (e.key !== grow && e.key !== shrink && e.key !== "Home") return;
      e.preventDefault();
      if (e.key === "Home") set(p, p.fallback ?? Math.round((globalThis.innerHeight || 800) * 0.47));
      else set(p, p.current(root) + (e.key === grow ? step : -step));
      ctx.drawTimeline();
    });
  }
  (_a = globalThis.addEventListener) == null ? void 0 : _a.call(globalThis, "resize", onResize);
  return () => {
    var _a2;
    return (_a2 = globalThis.removeEventListener) == null ? void 0 : _a2.call(globalThis, "resize", onResize);
  };
}
const paints = /* @__PURE__ */ new WeakMap();
function geometry(ctx, ev) {
  const timed = ev.filter((e) => e.at).sort((a, b) => a.at - b.at);
  const width = Math.round(Math.max(1280, timed.length * 104) * ctx.state.zoom);
  const usable = width - 42;
  const left = 20;
  if (!timed.length) return { width, bands: [], gaps: [], x: () => left };
  const runs = [];
  let cur = { from: timed[0].at, to: timed[0].at };
  for (const e of timed) {
    if (e.at - cur.to > QUIET_MS) {
      runs.push(cur);
      cur = { from: e.at, to: e.at };
    } else cur.to = e.at;
  }
  runs.push(cur);
  const active = runs.reduce((a, r) => a + Math.max(1, r.to - r.from), 0);
  const gapW = runs.length > 1 ? Math.min(90, usable * 0.07) : 0;
  const forBands = usable - gapW * (runs.length - 1);
  let x = left;
  const bands = [];
  const gaps = [];
  runs.forEach((r, i) => {
    const w = Math.max(24, forBands * Math.max(1, r.to - r.from) / active);
    bands.push({ from: r.from, to: r.to, left: x, width: w });
    x += w;
    if (i < runs.length - 1) {
      gaps.push({ left: x, width: gapW, ms: runs[i + 1].from - r.to });
      x += gapW;
    }
  });
  return {
    width: Math.max(width, x + 20),
    bands,
    gaps,
    x(t) {
      for (const b of bands) {
        if (t <= b.to) return b.left + (b.to === b.from ? 0 : (t - b.from) / (b.to - b.from) * b.width);
      }
      const last = bands[bands.length - 1];
      return last.left + last.width;
    }
  };
}
function layout(ctx, stream) {
  const { s, model: m } = ctx;
  const used = new Set(m.flow(stream).map((e) => e.track));
  const nested = m.foldersFor(stream).length > 0;
  const lanes = [];
  let y = 24;
  const streamY = y;
  let nestedY = null;
  const addNested = () => {
    nestedY = y;
    lanes.push({ key: "nested", label: s.laneNested, y, color: "var(--acv-kind-agent)", indent: true });
    y += LANE_H;
  };
  for (const k of TRACKS) {
    if (used.has(k)) {
      lanes.push({ key: k, label: s[TRACK_NAME[k]], y, indent: true });
      y += LANE_H;
    }
    if (k === "agents" && nested) addNested();
  }
  if (nested && nestedY === null) addNested();
  const rows = {};
  for (const l of lanes) rows[l.key] = l.y;
  return { lanes, rows, streamY, nestedY, height: y + 10 };
}
function drawLanes(ctx, L) {
  const { model: m, state } = ctx;
  const gutter = ctx.q(".acv-lane-labels");
  const st = state.stream ? m.streamByName.get(state.stream) : void 0;
  if (!st) {
    gutter.innerHTML = "";
    return;
  }
  const child = st.role === "main" ? "" : " child";
  let html = "";
  for (const l of L.lanes) {
    html += `<div class="acv-lane-label${l.indent ? child : ""}" style="top:${l.y}px${l.color ? `;color:${l.color}` : ""}">${esc(l.label)}</div>`;
  }
  html += `<div class="acv-lane-extent" style="top:${L.height - 1}px" aria-hidden="true"></div>`;
  gutter.innerHTML = html;
}
function drawTimeline(ctx) {
  var _a;
  const { s, f, model: m, state } = ctx;
  const stream = state.stream;
  const canvas = ctx.q(".acv-tl-canvas");
  const statics = ctx.q(".acv-tl-static");
  const scroll = ctx.q(".acv-tl-scroll");
  if (!stream) {
    statics.innerHTML = "";
    ctx.q(".acv-tl-items").innerHTML = "";
    paints.delete(ctx);
    return;
  }
  const L = layout(ctx, stream);
  drawLanes(ctx, L);
  const keepLeft = scroll.scrollLeft;
  const ev = m.flow(stream);
  const folders = m.foldersFor(stream);
  const g = geometry(ctx, ev);
  const st = m.streamByName.get(stream);
  ctx.q(".acv-tl-scope").textContent = st ? `${streamName(ctx, st.name)} · ${f.number(ev.length)} ${s.events}${folders.length ? ` · ${folders.length} ${s.nested}` : ""}` : "—";
  const up = st && st.role !== "main" ? m.openerOf(st.name) : null;
  const back = ctx.q(".acv-tl-back");
  back.hidden = !up && !state.navStack.length;
  if (up) {
    back.textContent = `↰ ${streamName(ctx, up.step.stream)}`;
    back.dataset.upStream = up.step.stream;
    back.dataset.upStep = up.step.id;
    back.dataset.upTalk = up.talk ?? "";
  } else {
    back.textContent = s.parentTimeline;
    delete back.dataset.upStream;
    delete back.dataset.upStep;
    delete back.dataset.upTalk;
  }
  const LADDER = [1, 2, 5, 10, 15, 30, 60, 120, 360, 720, 1440].map((min) => min * 6e4);
  const marks = [];
  for (const b of g.bands) {
    const perMs = b.width / Math.max(1, b.to - b.from);
    const every = LADDER.find((ms) => ms * perMs >= 100) ?? LADDER[LADDER.length - 1];
    for (let t = Math.ceil(b.from / every) * every; t <= b.to; t += every) {
      const prev = t - every;
      marks.push({ x: b.left + (t - b.from) * perMs, t, every, newDay: prev < b.from || f.day(t) !== f.day(prev), label: false });
    }
  }
  marks.sort((a, b) => a.x - b.x);
  let lastLabel = -1e9;
  for (const mk of marks) {
    mk.label = mk.x - lastLabel >= 92;
    if (mk.label) lastLabel = mk.x;
  }
  const known = /* @__PURE__ */ new Map();
  let lastKnown = 0;
  for (const e of m.steps(stream)) {
    if (e.at) lastKnown = e.at;
    else known.set(e.id, lastKnown);
  }
  const firstKnown = ((_a = m.flow(stream).find((e) => e.at)) == null ? void 0 : _a.at) ?? 0;
  const at = (e) => e.at || known.get(e.id) || firstKnown;
  const pos = /* @__PURE__ */ new Map();
  for (const e of ev) {
    const x = g.x(at(e));
    const y = L.rows[e.track] ?? L.streamY;
    pos.set(e.id, { x, w: 110, y, cx: x + 55, cy: y + 14 });
  }
  const lanes = /* @__PURE__ */ new Map();
  for (const e of ev) {
    const k = e.track;
    const arr = lanes.get(k);
    if (arr) arr.push(e);
    else lanes.set(k, [e]);
  }
  for (const group of lanes.values()) {
    let right = -1e9;
    for (const e of group) {
      const q = pos.get(e.id);
      if (q.x < right) {
        q.x = right;
        q.cx = q.x + q.w / 2;
      }
      right = q.x + 24;
    }
    group.sort((a, b) => pos.get(a.id).x - pos.get(b.id).x);
    for (let i = 0; i < group.length - 1; i++) {
      const a = pos.get(group[i].id);
      const b = pos.get(group[i + 1].id);
      const room = b.x - a.x - 3;
      if (a.w > room) {
        a.w = Math.max(30, room);
        a.cx = a.x + a.w / 2;
      }
    }
  }
  const groups = /* @__PURE__ */ new Map();
  for (const fo of folders) {
    const arr = groups.get(fo.from.id);
    if (arr) arr.push(fo);
    else groups.set(fo.from.id, [fo]);
  }
  const shown = [...groups.values()].map((list) => ({
    ...list[0],
    count: list.length,
    key: list.length > 1 ? `group:${list[0].from.id}` : list[0].stream.name
  }));
  const fpos = /* @__PURE__ */ new Map();
  let packed = 0;
  for (const fo of shown.sort((a, b) => (a.from.at || 0) - (b.from.at || 0))) {
    const w = 132;
    const x = Math.max(fo.from.at ? g.x(fo.from.at) + 24 : 20, packed);
    packed = x + w + 5;
    fpos.set(fo.key, { x, w, y: L.nestedY ?? L.streamY, cx: x + w / 2, cy: (L.nestedY ?? L.streamY) + 16 });
  }
  let canvasW = Math.max(g.width, packed + 40);
  for (const q of pos.values()) canvasW = Math.max(canvasW, q.x + q.w + 40);
  for (const q of fpos.values()) canvasW = Math.max(canvasW, q.x + q.w + 40);
  let html = "";
  g.bands.forEach((b, i) => {
    html += `<div class="acv-band${i % 2 ? " alt" : ""}" style="left:${b.left}px;width:${Math.max(1, b.width)}px"></div>`;
  });
  for (const gp of g.gaps) {
    html += `<div class="acv-gap" style="left:${gp.left}px;width:${gp.width}px"></div>`;
    html += `<div class="acv-gap-label" style="left:${gp.left + gp.width / 2}px">${esc(f.duration(gp.ms))} ${esc(s.quiet)}</div>`;
  }
  for (const l of L.lanes) html += `<div class="acv-track-line" style="top:${l.y - 5}px"></div>`;
  html += edgesSvg(ctx, ev, pos, shown, fpos, canvasW, L.height);
  statics.innerHTML = html;
  canvas.style.width = `${canvasW}px`;
  canvas.style.height = `${L.height}px`;
  paints.set(ctx, {
    stream,
    ev,
    pos,
    marks,
    folders: shown,
    fpos,
    canvasW,
    height: L.height,
    near: m.relatedTo(state.picked && state.focus ? state.sel : null, state.folder, ev)
  });
  scroll.scrollLeft = keepLeft;
  paintViewport(ctx);
}
function edgesSvg(ctx, ev, pos, shown, fpos, w, h) {
  const { state } = ctx;
  let html = `<svg class="acv-edges" viewBox="0 0 ${w} ${h}"><defs>
    <marker id="acv-arrow-exact" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="5" markerHeight="5" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" class="acv-arrow exact"></path></marker>
    <marker id="acv-arrow-inferred" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="5" markerHeight="5" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" class="acv-arrow inferred"></path></marker>
    <marker id="acv-arrow-weak" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="5" markerHeight="5" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" class="acv-arrow weak"></path></marker>
    <marker id="acv-arrow-owns" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="4" markerHeight="4" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" class="acv-arrow owns"></path></marker></defs>`;
  if (!state.sel && !state.folder) return `${html}</svg>`;
  const byId = new Map(ev.map((e) => [e.id, e]));
  for (const e of ev) {
    const parent = e.parent ? byId.get(e.parent) : void 0;
    if (!parent || parent.track === e.track) continue;
    if (e.id !== state.sel && parent.id !== state.sel) continue;
    const a = pos.get(parent.id);
    const b = pos.get(e.id);
    if (!a || !b) continue;
    const down = a.cy < b.cy;
    const sx = a.cx;
    const sy = a.cy + (down ? 14 : -14);
    const ex = b.cx;
    const ey = b.cy + (down ? -14 : 14);
    const lift = Math.max(10, Math.abs(ey - sy) * 0.5);
    html += `<path class="acv-owns selected" d="M ${sx} ${sy} C ${sx} ${sy + (down ? lift : -lift)}, ${ex} ${ey - (down ? lift : -lift)}, ${ex} ${ey}"></path>`;
  }
  const onScreen = new Set(ev.map((e) => e.id));
  const drawn = /* @__PURE__ */ new Set();
  for (const e of ev) {
    for (const rel of e.edges) {
      if (rel.dir !== "out" || !onScreen.has(rel.other)) continue;
      if (e.id !== state.sel && rel.other !== state.sel) continue;
      const key = `${e.id}>${rel.other}`;
      if (drawn.has(key)) continue;
      drawn.add(key);
      const a = pos.get(e.id);
      const b = pos.get(rel.other);
      if (!a || !b) continue;
      const sx = a.x + a.w;
      const sy = a.cy;
      const ex = b.x;
      const ey = b.cy;
      const bend = Math.max(18, Math.abs(ex - sx) * 0.42);
      const cls = rel.quality === "exact_unique" ? "" : rel.quality === "strong_inference" ? " inferred" : " weak";
      html += `<path class="acv-edge${cls} selected" d="M ${sx} ${sy} C ${sx + bend} ${sy}, ${ex - bend} ${ey}, ${ex} ${ey}"></path>`;
    }
  }
  for (const fo of shown) {
    if (state.folder ? state.folder !== fo.stream.name : fo.from.id !== state.sel) continue;
    const a = pos.get(fo.from.id);
    const b = fpos.get(fo.key);
    if (!a || !b) continue;
    const sx = fo.back ? b.x + 26 : a.cx;
    const sy = fo.back ? b.y : a.y + 28;
    const ex = fo.back ? a.cx : b.x + 26;
    const ey = fo.back ? a.y + 28 : b.y;
    const lift = Math.max(12, Math.abs(ey - sy) * 0.5);
    const down = sy < ey ? 1 : -1;
    const cls = fo.quality === "exact_unique" ? "" : fo.quality === "strong_inference" ? " inferred" : " weak";
    html += `<path class="acv-edge drop${cls} selected" d="M ${sx} ${sy} C ${sx} ${sy + down * lift}, ${ex} ${ey - down * lift}, ${ex} ${ey}"></path>`;
  }
  return `${html}</svg>`;
}
function paintViewport(ctx) {
  const p = paints.get(ctx);
  if (!p) return;
  const { s, f, state } = ctx;
  const scroll = ctx.q(".acv-tl-scroll");
  const view = scroll.clientWidth;
  const lo = view ? scroll.scrollLeft - view : -Infinity;
  const hi = view ? scroll.scrollLeft + 2 * view : Infinity;
  const inView = (x, w) => x + w >= lo && x <= hi;
  let html = "";
  for (const mk of p.marks) {
    if (!inView(mk.x, 100)) continue;
    const stamp = mk.every >= 6e4 ? f.timeShort(mk.t) : f.time(mk.t);
    html += `<div class="acv-tick" style="left:${mk.x}px">${mk.label ? `<span>${mk.newDay ? `${esc(f.day(mk.t))} ` : ""}${esc(stamp)}</span>` : ""}</div>`;
  }
  for (const e of p.ev) {
    const q = p.pos.get(e.id);
    if (!inView(q.x, q.w)) continue;
    const says = e.kind === "context.injection" ? injectionSays(e.text) : null;
    html += `<button type="button" class="acv-clip acv-kind-${e.type}${e.id === state.sel ? " selected" : ""}${p.near && !p.near.has(e.id) ? " dim" : ""}" data-node="${esc(e.id)}" data-talk="${esc(e.talk ?? "")}" title="${esc(e.at ? `${f.time(e.at)} · ` : "")}${esc(kindTitle(e.kind, s))}${e.name ? ` · ${esc(e.name)}` : ""}" style="left:${q.x}px;top:${q.y}px;width:${q.w}px">${esc(says ? says.says : e.name || kindTitle(e.kind, s))}</button>`;
  }
  for (const fo of p.folders) {
    const q = p.fpos.get(fo.key);
    if (!inView(q.x, q.w)) continue;
    if (fo.count > 1) {
      html += `<button type="button" class="acv-clip acv-kind-agent nested${fo.from.id === state.sel ? " selected" : ""}" data-group="${esc(fo.from.id)}" title="${esc(fill(s.poolTitle, { n: fo.count }))}" style="left:${q.x}px;top:${q.y}px;width:${q.w}px">${fo.count} ${esc(s.agentsCreated)}</button>`;
      continue;
    }
    html += `<button type="button" class="acv-clip acv-kind-agent nested${state.folder === fo.stream.name ? " selected" : ""}${p.near && state.folder && state.folder !== fo.stream.name ? " dim" : ""}" data-folder="${esc(fo.stream.name)}" title="${esc(fo.stream.label || fo.stream.name)} — ${esc(fo.quality)}. ${esc(fo.back ? s.reportedHere : s.clickToSeeOpener)}" style="left:${q.x}px;top:${q.y}px;width:${q.w}px">${esc(
      fo.stream.label || `${s.childStreamSingular} ${fo.stream.name.slice(0, 6)}`
    )}</button>`;
  }
  const ph = state.sel ? p.pos.get(state.sel) : void 0;
  if (ph) html += `<div class="acv-playhead" style="left:${ph.cx}px"></div>`;
  ctx.q(".acv-tl-items").innerHTML = html;
}
function centerX(ctx, id) {
  const p = paints.get(ctx);
  const q = p == null ? void 0 : p.pos.get(id);
  return q ? q.cx : null;
}
function folderBottom(ctx, name) {
  const p = paints.get(ctx);
  if (!p) return null;
  const fo = p.folders.find((x) => x.stream.name === name);
  const q = fo ? p.fpos.get(fo.key) : void 0;
  return q ? q.y + 32 : null;
}
function bindTimeline(ctx) {
  const scroll = ctx.q(".acv-tl-scroll");
  const canvas = ctx.q(".acv-tl-canvas");
  canvas.addEventListener("click", (ev) => {
    const target = ev.target;
    const node = target.closest("[data-node]");
    if (node) {
      const tid = node.dataset.talk || null;
      if (tid && (!ctx.state.talk || ctx.state.talk.id !== tid)) ctx.openTalk(tid, true);
      for (const id of [...ctx.state.autoOpen]) {
        if (id !== tid) {
          ctx.state.openTalks.delete(id);
          ctx.state.autoOpen.delete(id);
        }
      }
      if (tid && !ctx.state.openTalks.has(tid)) {
        ctx.state.openTalks.add(tid);
        ctx.state.autoOpen.add(tid);
      }
      ctx.select(node.dataset.node, true);
      return;
    }
    const folder = target.closest("[data-folder]");
    if (folder) {
      ev.stopPropagation();
      ctx.selectFolder(folder.dataset.folder);
      return;
    }
    const group = target.closest("[data-group]");
    if (group) {
      ev.stopPropagation();
      ctx.select(group.dataset.group, true);
      ctx.showTab("relations");
      return;
    }
    if (target.closest("button, a, select")) return;
    ctx.clearSelection();
  });
  let queued = false;
  scroll.addEventListener("scroll", () => {
    ctx.q(".acv-lane-labels").scrollTop = scroll.scrollTop;
    if (queued) return;
    queued = true;
    const g = globalThis;
    const run = () => {
      queued = false;
      paintViewport(ctx);
    };
    if (typeof g.requestAnimationFrame === "function") g.requestAnimationFrame(run);
    else setTimeout(run, 0);
  });
  ctx.q(".acv-lane-labels").addEventListener(
    "wheel",
    (e) => {
      if (!e.deltaY) return;
      e.preventDefault();
      scroll.scrollTop += e.deltaY;
    },
    { passive: false }
  );
}
function present(ctx, e) {
  const { s, model: m } = ctx;
  const st = m.streamByName.get(e.stream);
  const agent = !st ? s.agent : st.role === "main" ? s.mainAgent : st.label || s.childAgent;
  const child = st && st.role !== "main" ? " child" : "";
  switch (e.kind) {
    case "message.external":
      return { cls: "acv-message acv-human", role: s.externalInput, color: "user" };
    case "message.assistant":
      return { cls: "acv-message acv-agent", role: `${agent} · ${s.response}`, color: "assistant" };
    case "agent.output":
      return { cls: "acv-message acv-subagent", role: `${agent} · ${s.streamOutput}`, color: "subagent-response" };
    case "message.synthetic":
      return { cls: "acv-message acv-agent", role: s.clientMadeMessage, color: "error" };
    case "context.injection": {
      const w = injectionSays(e.text);
      return { cls: "acv-activity acv-instruction", role: `${s.contextPutIn}${(w == null ? void 0 : w.key) ? ` · ${w.key}` : ""}`, color: "instruction" };
    }
    case "agent.call":
      return { cls: "acv-activity acv-handoff", role: `${s.agentCallToChild} · ${agent} → ${s.childAgent.toLowerCase()}`, color: "agent" };
    case "runtime.notification":
      return { cls: "acv-activity acv-handoff", role: `${s.notificationFromChild} · ${s.childAgent.toLowerCase()} → ${agent}`, color: "agent" };
    case "agent.launch_ack":
      return { cls: `acv-activity${child}`, role: `${agent} · ${s.launchAcknowledged}`, color: "agent" };
    case "llm.call":
      return { cls: `acv-activity${child}`, role: `${agent} · ${s.modelCall}`, color: "model" };
    case "thinking":
      return { cls: `acv-activity${child}`, role: `${agent} · ${s.reasoning}`, color: "model" };
    case "tool":
      return { cls: `acv-activity${child}`, role: `${agent} → ${s.toolWord} · ${e.name || s.callWord}`, color: "tool" };
    default:
      return { cls: `acv-activity${child}`, role: `${agent} · ${kindTitle(e.kind, s)}`, color: "instruction" };
  }
}
function stepCard(ctx, e, prev, near) {
  const { s, f, model: m, state } = ctx;
  let sep = "";
  if (prev && e.at && prev.at && e.at - prev.at > QUIET_MS) {
    sep = `<div class="acv-separator dormant">${esc(f.duration(e.at - prev.at))} ${esc(s.quiet)}</div>`;
  }
  const p = present(ctx, e);
  const mono = e.kind === "tool" || e.kind === "context.injection";
  const indent = Math.max(0, (e.depth || 1) - 1);
  const folder = e.edges.find((g) => g.type === "starts" && g.dir === "out");
  const fs = folder ? m.streamById.get(folder.other) : void 0;
  const link = fs ? `<button type="button" class="acv-stream-link" data-open="${esc(fs.name)}">${esc(s.openStream)} ${esc(fs.label || fs.name.slice(0, 10))} →</button>` : "";
  const unavailable = e.state && e.state !== "available" ? `<span class="acv-mini acv-warn">[${esc(e.state)}]</span>` : "";
  const title = e.durationMs ? `${f.duration(e.durationMs)} ${s.turn}` : e.kind === "context.injection" && injectionSays(e.text) ? injectionSays(e.text).says : e.name || kindTitle(e.kind, s);
  return `${sep}<button type="button" class="acv-card ${p.cls}${e.id === state.sel ? " selected" : ""}${near && !near.has(e.id) ? " dim" : ""}" data-card="${esc(e.id)}" title="${esc(s.locateInTimeline)}"${indent ? ` style="--indent:${indent};margin-left:${indent * 22}px;max-width:calc(100% - ${indent * 22}px)"` : ""}>
    <span class="acv-time"><strong>${esc(f.time(e.at))}</strong>${esc(e.kind)}</span>
    <span class="acv-content">
      <span class="acv-role">${esc(p.role)}</span>
      <span class="acv-title acv-kind-${p.color}"><span class="acv-type-mark"></span>${esc(title)}
        <span class="acv-mini">${e.bytes ? `${f.number(e.bytes)} B` : ""}</span> ${unavailable}</span>
      ${e.text ? `${e.result ? `<span class="acv-result-label">${esc(s.input)}${e.bytes ? ` · ${f.number(e.bytes)} B` : ""}</span>` : ""}
        <span class="acv-text${mono ? " mono" : ""}${e.text.length > 380 ? " clamped" : ""}">${esc(e.text)}</span>` : ""}
      ${e.result ? `<span class="acv-result-block"><span class="acv-result-label">${esc(s.result)}${e.failed ? ` · ${esc(s.failed)}` : ""}${e.resultBytes ? ` · ${f.number(e.resultBytes)} B` : ""}${e.reqToRes != null ? ` · ${esc(f.duration(e.reqToRes))} ${esc(s.toReturn)}` : ""}</span>
        <span class="acv-text mono result${e.result.length > 380 ? " clamped" : ""}">${esc(e.result)}</span></span>` : ""}
    </span></button>${link}`;
}
function talkCards(ctx, t, prevTo) {
  var _a;
  const { s, f, model: m, state } = ctx;
  const st = m.streamByName.get(t.stream);
  const isOpen = state.openTalks.has(t.id);
  const focused = ((_a = state.talk) == null ? void 0 : _a.id) === t.id;
  const talkSteps = m.stepsOfTalk(t.id);
  const opening = talkSteps.find((e) => e.kind === "message.external") ?? talkSteps[0];
  const replies = talkSteps.filter((e) => e.kind === "message.assistant" || e.kind === "agent.output");
  const reply = replies[replies.length - 1];
  let html = "";
  if (prevTo && t.from && t.from - prevTo > QUIET_MS) {
    html += `<div class="acv-separator dormant">${esc(f.duration(t.from - prevTo))} ${esc(s.quiet)}</div>`;
  }
  html += `<button type="button" class="acv-card acv-message acv-human${focused ? " focused" : ""}" data-talk="${esc(t.id)}"${opening ? ` data-step="${esc(opening.id)}"` : ""} title="${esc(s.showThisTalk)}">
    <span class="acv-time"><strong>${esc(f.time(t.from))}</strong>${esc(f.day(t.from))}</span>
    <span class="acv-content"><span class="acv-role">${esc(s.externalInput)}</span>
      <span class="acv-text${t.label.length > 380 ? " clamped" : ""}">${esc(t.label || (t.child ? s.delegatedWork : s.noOpeningLine))}</span></span></button>`;
  if (t.steps) {
    const bits = [`${f.number(t.steps)} ${s.steps.toLowerCase()}`];
    if (t.runs) bits.push(`${t.runs} ${s.runs}`);
    if (t.tools) bits.push(`${f.number(t.tools)} ${s.tools}`);
    if (t.to > t.from) bits.push(f.duration(t.to - t.from));
    html += `<button type="button" class="acv-fold" data-work="${esc(t.id)}" aria-expanded="${isOpen}">
      <span class="acv-fold-mark">${isOpen ? "▾" : "▸"}</span>
      <span class="acv-fold-label">${esc(isOpen ? s.hideWork : s.showWork)}</span>
      <span class="acv-fold-stat">${esc(bits.join(" · "))}</span></button>`;
    if (isOpen) {
      const ev = m.stepsOfTalk(t.id);
      const near = m.relatedTo(state.picked && state.focus ? state.sel : null, state.folder, m.steps(t.stream));
      const closing = t.reply ? [...ev].reverse().find((e) => e.kind === "message.assistant" || e.kind === "agent.output") : void 0;
      const work = ev.filter((e) => e.kind !== "message.external" && e !== closing);
      html += `<div class="acv-fold-body">${work.map((w, i) => stepCard(ctx, w, work[i - 1], near)).join("")}</div>`;
      html += `<button type="button" class="acv-fold" data-work="${esc(t.id)}" aria-expanded="true">
        <span class="acv-fold-mark">▴</span><span class="acv-fold-label">${esc(s.hideWork)}</span></button>`;
    }
  }
  if (t.reply) {
    html += `<button type="button" class="acv-card acv-message acv-agent${focused ? " focused" : ""}" data-talk="${esc(t.id)}" data-end="reply"${reply ? ` data-step="${esc(reply.id)}"` : ""} title="${esc(s.showThisAnswer)}">
      <span class="acv-time"><strong>${esc(f.time(t.to))}</strong></span>
      <span class="acv-content">
        <span class="acv-role">${esc(st && st.role === "main" ? s.mainAgent : s.childAgent)} · ${esc(s.response)}</span>
        <span class="acv-text${t.reply.length > 380 ? " clamped" : ""}">${esc(t.reply)}</span>
      </span></button>`;
  }
  return html;
}
function drawTranscript(ctx) {
  const { s, model: m, state } = ctx;
  const list = ctx.q(".acv-transcript-list");
  const stream = state.stream;
  const nothing = m.doc.summary.steps === 0 && m.doc.summary.problems.length > 0;
  if (!stream) {
    list.innerHTML = `<div class="acv-empty">${esc(nothing ? s.nothingFolded : s.noTalksInStream)}</div>`;
    return;
  }
  const talks = m.talksOf(stream);
  const loose = m.loose(stream);
  const st = m.streamByName.get(stream);
  const opened = st && st.role !== "main" ? m.openerOf(st.name) : null;
  let html = "";
  if (st && st.role !== "main") {
    html += `<div class="acv-stream-banner"><span><strong>${esc(st.label || st.name)}</strong><br>
      ${esc(s.independentStream)}
      ${opened ? `${esc(s.openedFrom)} <b>${esc(streamName(ctx, opened.step.stream))}</b> · ${esc(opened.quality)}.` : esc(s.noOpenerRecorded)}</span>
      ${opened ? `<button type="button" data-opener-stream="${esc(opened.step.stream)}" data-opener-step="${esc(opened.step.id)}" data-opener-talk="${esc(opened.talk ?? "")}">${esc(s.backToOpener)}</button>` : ""}</div>`;
  }
  if (!talks.length && !loose.length) {
    list.innerHTML = html + `<div class="acv-empty">${esc(nothing ? s.nothingFolded : s.noTalksInStream)}</div>`;
    return;
  }
  let prevTo = null;
  for (const t of talks) {
    html += talkCards(ctx, t, prevTo);
    prevTo = t.to || prevTo;
  }
  if (loose.length) {
    const near = m.relatedTo(state.picked && state.focus ? state.sel : null, state.folder, m.steps(stream));
    html += `<div class="acv-separator">${esc(s.outsideAnyTalk)}</div>
      <div class="acv-loose-note">${esc(s.outsideAnyTalkNote)}</div>
      <div class="acv-fold-body">${loose.map((w, i) => stepCard(ctx, w, loose[i - 1], near)).join("")}</div>`;
  }
  list.innerHTML = html;
}
function bindTranscript(ctx) {
  const list = ctx.q(".acv-transcript-list");
  list.addEventListener("click", (ev) => {
    const target = ev.target;
    const fold = target.closest("[data-work]");
    if (fold) {
      ev.stopPropagation();
      const id = fold.dataset.work;
      ctx.state.autoOpen.delete(id);
      if (ctx.state.openTalks.has(id)) ctx.state.openTalks.delete(id);
      else ctx.state.openTalks.add(id);
      ctx.focusTalk(id);
      ctx.drawTranscript();
      return;
    }
    const open = target.closest("[data-open]");
    if (open) {
      ev.stopPropagation();
      ctx.selectFolder(open.dataset.open);
      return;
    }
    const card = target.closest("[data-card]");
    if (card) {
      ev.stopPropagation();
      ctx.select(card.dataset.card, true, false);
      return;
    }
    const opener = target.closest("[data-opener-stream]");
    if (opener) {
      ctx.goToOpener(opener.dataset.openerStream, opener.dataset.openerStep, opener.dataset.openerTalk || null);
      return;
    }
    const talk = target.closest("[data-talk]");
    if (talk) {
      ctx.focusTalk(talk.dataset.talk, talk.dataset.end);
      return;
    }
    ctx.clearFolder();
  });
}
const ASZ_VIEW_FORMAT = "asz.view";
const ASZ_VIEW_MAJOR_VERSION = 1;
function isSupportedDocument(v) {
  if (!v || typeof v !== "object") return false;
  const d = v;
  if (d.format !== ASZ_VIEW_FORMAT || typeof d.version !== "string") return false;
  const major = Number(d.version.split(".")[0]);
  return major === ASZ_VIEW_MAJOR_VERSION && Array.isArray(d.talks);
}
function skeleton(s) {
  return `
  <div class="acv-strip"><div class="acv-status"></div>
    <div class="acv-problems acv-explain" hidden>
      <div class="acv-explain-head"><span class="acv-explain-title acv-problems-title"></span><button type="button" class="acv-explain-close acv-problems-close" aria-label="${esc(s.close)}">×</button></div>
      <div class="acv-problems-body"></div>
    </div>
    <div class="acv-overview" id="acv-overview" hidden>
      <section class="acv-summary" aria-label="${esc(s.overview)}"></section>
      <div class="acv-picker">
        <div class="acv-picker-head"><span class="acv-kicker">${esc(s.talkList)}</span><input class="acv-talk-filter" type="search" placeholder="${esc(s.filterTalks)}" aria-label="${esc(s.filterTalks)}"></div>
        <div class="acv-talk-list"></div>
      </div>
    </div>
  </div>
  <section class="acv-workbench">
    <div class="acv-main">
      <section class="acv-transcript">
        <div class="acv-transcript-head">
          <div class="acv-heading"><h2>${esc(s.talk)}</h2><span class="acv-talk-caption"></span></div>
          <div class="acv-stream-tabs" role="tablist"></div>
        </div>
        <div class="acv-transcript-list"></div>
      </section>
      <div class="acv-splitter acv-split-inspector" role="separator" aria-orientation="vertical" tabindex="0"></div>
      <aside class="acv-inspector">
        <div class="acv-inspector-head">
          <div class="acv-heading"><span class="acv-kicker">${esc(s.inspector)}</span><h2 class="acv-inspector-title">—</h2></div>
          <div class="acv-inspector-meta"></div>
        </div>
        <div class="acv-tablist" role="tablist">
          <button class="acv-tab" type="button" role="tab" data-tab="details" aria-selected="true">${esc(s.details)}</button>
          <button class="acv-tab" type="button" role="tab" data-tab="relations" aria-selected="false">${esc(s.relations)}</button>
          <button class="acv-tab" type="button" role="tab" data-tab="evidence" aria-selected="false">${esc(s.evidence)}</button>
        </div>
        <div class="acv-inspector-body" role="tabpanel"></div>
      </aside>
    </div>
    <div class="acv-splitter acv-split-dock" role="separator" aria-orientation="horizontal" tabindex="0"></div>
    <section class="acv-dock">
      <div class="acv-toolbar">
        <div class="acv-control-group">
          <div class="acv-tl-heading"><strong>${esc(s.flowTimeline)} <button type="button" class="acv-q acv-dock-help-btn" aria-label="${esc(s.timelineHelp)}" title="${esc(s.timelineHelp)}">?</button></strong><small class="acv-tl-scope">—</small></div>
          <button type="button" class="acv-tl-back acv-btn" hidden>${esc(s.parentTimeline)}</button>
          <label class="acv-checkbox" title="${esc(s.fadeUnrelatedTitle)}"><input class="acv-focus" type="checkbox" checked> ${esc(s.fadeUnrelated)}</label>
        </div>
        <div class="acv-control-group">
          <label class="acv-control-label">${esc(s.zoom)}</label>
          <input class="acv-zoom" type="range" min="10" max="200" step="10" value="100" aria-label="${esc(s.zoom)}">
          <output class="acv-zoom-out">100%</output>
          <button type="button" class="acv-fit acv-btn" title="${esc(s.centerSelectedTitle)}">${esc(s.centerSelected)}</button>
        </div>
      <div class="acv-dock-help acv-explain" hidden>
          <div class="acv-explain-head"><span class="acv-explain-title">${esc(s.flowTimeline)}</span><button type="button" class="acv-explain-close acv-dock-help-close" aria-label="${esc(s.close)}">×</button></div>
          <dl class="acv-explain-rows">
            <dt>${esc(s.helpWhat)}</dt><dd>${esc(s.helpWhatText)}</dd>
            <dt>${esc(s.helpHeading)}</dt><dd>${esc(s.helpHeadingText)}</dd>
            <dt>${esc(s.helpAxis)}</dt><dd>${esc(s.helpAxisText)}</dd>
            <dt>${esc(s.helpLinks)}</dt><dd>${esc(s.helpLinksText)}</dd>
            <dt>${esc(s.helpFade)}</dt><dd>${esc(s.helpFadeText)}</dd>
          </dl>
        </div>
      </div>
      <div class="acv-tl-panel"><div class="acv-tl-layout">
        <div class="acv-lane-labels"></div>
        <div class="acv-tl-scroll"><div class="acv-tl-canvas"><div class="acv-tl-static"></div><div class="acv-tl-items"></div></div></div>
      </div></div>
      <div class="acv-legend">
        <span class="acv-legend-item acv-kind-user"><span class="acv-legend-swatch"></span>${esc(s.legendInput)}</span>
        <span class="acv-legend-item acv-kind-assistant"><span class="acv-legend-swatch"></span>${esc(s.legendResponse)}</span>
        <span class="acv-legend-item acv-kind-model"><span class="acv-legend-swatch"></span>${esc(s.legendModel)}</span>
        <span class="acv-legend-item acv-kind-tool"><span class="acv-legend-swatch"></span>${esc(s.legendTool)}</span>
        <span class="acv-legend-item acv-kind-agent"><span class="acv-legend-swatch"></span>${esc(s.legendAgent)}</span>
        <span class="acv-legend-item acv-kind-instruction"><span class="acv-legend-swatch"></span>${esc(s.legendContext)}</span>
        <span class="acv-legend-item" style="color:var(--acv-owns)"><span class="acv-legend-line"></span>${esc(s.legendOwns)}</span>
        <span class="acv-legend-item" style="color:var(--acv-edge)"><span class="acv-legend-line"></span>${esc(s.legendExact)}</span>
        <span class="acv-legend-item"><span class="acv-legend-line dashed"></span>${esc(s.legendInferred)}</span>
      </div>
    </section>
  </section>
  <div class="acv-sr" aria-live="polite"></div>`;
}
function mountConversationView(host, opts) {
  const s = { ...ENGLISH, ...opts.strings ?? {} };
  const model = new ConversationModel(opts.document);
  const root = host;
  root.classList.add("acv");
  root.innerHTML = skeleton(s);
  const state = {
    talk: null,
    stream: null,
    sel: null,
    folder: null,
    rawRef: null,
    zoom: 1,
    focus: true,
    picked: false,
    tab: "details",
    navStack: [],
    openTalks: /* @__PURE__ */ new Set(),
    autoOpen: /* @__PURE__ */ new Set(),
    explain: null,
    overviewOpen: false
  };
  let muted = 0;
  const emit = () => {
    var _a;
    if (muted === 0) (_a = opts.onStateChange) == null ? void 0 : _a.call(opts, getState());
  };
  const getState = () => ({
    ...state.talk ? { talk: state.talk.id } : {},
    ...state.sel ? { step: state.sel } : {},
    ...state.stream ? { stream: state.stream } : {}
  });
  const ctx = {
    root,
    model,
    s,
    f: opts.formatter ?? EN_US_FORMATTER,
    glossary: opts.glossary ?? null,
    loadRecord: opts.loadRecord,
    state,
    q(selector) {
      const el = root.querySelector(selector);
      if (!el) throw new Error(`conversation-view: missing element ${selector}`);
      return el;
    },
    select,
    selectFolder,
    clearSelection,
    clearFolder,
    diveIn,
    goBack,
    goToOpener,
    openTalk,
    focusTalk,
    switchStream,
    showTab,
    renderAll,
    drawTimeline: () => drawTimeline(ctx),
    drawTranscript: () => drawTranscript(ctx),
    drawInspector: () => drawInspector(ctx),
    drawStreamTabs: () => drawStreamTabs(ctx),
    drawTalkList: () => drawTalkList(ctx),
    centerOn,
    announce: (text) => {
      ctx.q(".acv-sr").textContent = text;
    }
  };
  function renderAll(recenter = false) {
    drawStreamTabs(ctx);
    drawTimeline(ctx);
    drawTranscript(ctx);
    drawInspector(ctx);
    if (recenter) centerOn(state.sel, "auto");
    emit();
  }
  function openTalk(id, keepStack = false) {
    var _a, _b;
    const t = model.talkById.get(id);
    if (!t) return;
    state.talk = t;
    if (!keepStack) state.navStack = [];
    state.stream = t.stream;
    state.picked = false;
    state.folder = null;
    state.openTalks.clear();
    state.autoOpen.clear();
    const ev = model.stepsOfTalk(t.id);
    state.sel = ((_a = ev[0]) == null ? void 0 : _a.id) ?? ((_b = model.steps(t.stream)[0]) == null ? void 0 : _b.id) ?? null;
    drawTalkList(ctx);
    renderAll(true);
  }
  function focusTalk(id, end) {
    const t = model.talkById.get(id);
    if (!t) return;
    state.talk = t;
    state.stream = t.stream;
    state.picked = false;
    const steps = model.stepsOfTalk(id);
    let op;
    if (end === "reply") {
      const replies = steps.filter((e) => e.kind === "message.assistant" || e.kind === "agent.output");
      op = replies[replies.length - 1] ?? steps[steps.length - 1];
    } else {
      op = steps.find((e) => e.kind === "message.external") ?? steps[0];
    }
    if (op) state.sel = op.id;
    drawStreamTabs(ctx);
    drawTimeline(ctx);
    drawTranscript(ctx);
    drawInspector(ctx);
    if (op) centerOn(op.id, reducedMotion() ? "auto" : "smooth", false);
    emit();
  }
  function switchStream(name) {
    var _a, _b;
    if (state.stream === name) return;
    if (!model.streamByName.has(name)) return;
    state.stream = name;
    const ev = model.steps(name);
    if (ev.length && !ev.some((e) => e.id === state.sel)) state.sel = ev[0].id;
    const talkId = ((_a = model.step(state.sel)) == null ? void 0 : _a.talk) ?? ((_b = model.talksOf(name)[0]) == null ? void 0 : _b.id) ?? null;
    state.talk = talkId ? model.talkById.get(talkId) ?? null : null;
    renderAll(true);
  }
  function select(id, center, alsoTranscript = true) {
    var _a;
    const e = model.step(id);
    if (!e) return;
    if (e.stream !== state.stream) {
      switchStream(e.stream);
    }
    if (e.talk && ((_a = state.talk) == null ? void 0 : _a.id) !== e.talk) {
      const t = model.talkById.get(e.talk);
      if (t) state.talk = t;
    }
    state.sel = id;
    state.rawRef = null;
    state.folder = null;
    state.picked = true;
    drawStreamTabs(ctx);
    drawTimeline(ctx);
    drawTranscript(ctx);
    drawInspector(ctx);
    if (center) centerOn(id, reducedMotion() ? "auto" : "smooth", alsoTranscript);
    ctx.announce(fill(s.selected, { what: `${e.kind}${e.name ? ` ${e.name}` : ""}` }));
    emit();
  }
  function selectFolder(name) {
    if (!state.stream) return;
    const f = model.foldersFor(state.stream).find((x) => x.stream.name === name);
    if (!f) return;
    state.folder = name;
    state.picked = true;
    state.sel = f.from.id;
    state.rawRef = null;
    drawTimeline(ctx);
    drawTranscript(ctx);
    drawInspector(ctx);
    centerOn(f.from.id, reducedMotion() ? "auto" : "smooth");
    const bottom = folderBottom(ctx, name);
    if (bottom !== null) {
      const sc = ctx.q(".acv-tl-scroll");
      const want = Math.max(0, bottom - sc.clientHeight + 12);
      if (want > sc.scrollTop) {
        sc.scrollTop = want;
        ctx.q(".acv-lane-labels").scrollTop = want;
      }
    }
    ctx.announce(fill(s.selectedNestedStream, { name: f.stream.label || name, kind: f.from.kind }));
    emit();
  }
  function clearSelection() {
    if (!state.folder && !state.sel && !state.picked) return;
    state.folder = null;
    state.sel = null;
    state.picked = false;
    state.rawRef = null;
    drawTimeline(ctx);
    drawTranscript(ctx);
    drawInspector(ctx);
    ctx.announce(s.selectionCleared);
    emit();
  }
  function clearFolder() {
    if (!state.folder) return;
    state.folder = null;
    state.picked = false;
    drawTimeline(ctx);
    drawTranscript(ctx);
    drawInspector(ctx);
  }
  function diveIn() {
    var _a;
    const name = state.folder;
    if (!name || !state.stream) return;
    state.navStack.push({ stream: state.stream, sel: state.sel, talk: ((_a = state.talk) == null ? void 0 : _a.id) ?? null });
    state.folder = null;
    switchStream(name);
  }
  function goBack() {
    const t = state.navStack.pop();
    if (!t) return;
    state.stream = t.stream;
    state.sel = t.sel;
    state.talk = t.talk ? model.talkById.get(t.talk) ?? null : null;
    renderAll(true);
  }
  function goToOpener(stream, step, talk) {
    var _a;
    muted++;
    try {
      if (talk && ((_a = state.talk) == null ? void 0 : _a.id) !== talk) openTalk(talk, true);
      else if (stream !== state.stream) switchStream(stream);
      state.folder = null;
      select(step, true);
    } finally {
      muted--;
    }
    emit();
  }
  function showTab(tab) {
    state.tab = tab;
    root.querySelectorAll("[data-tab]").forEach((t) => t.setAttribute("aria-selected", String(t.dataset.tab === tab)));
    drawInspector(ctx);
  }
  function centerOn(id, behavior, alsoTranscript = true) {
    if (!id) return;
    const sc = ctx.q(".acv-tl-scroll");
    const cx = centerX(ctx, id);
    if (cx !== null) {
      scrollTo(sc, { left: Math.max(0, cx - sc.clientWidth / 2), behavior });
      paintViewport(ctx);
    }
    if (!alsoTranscript) return;
    const list = ctx.q(".acv-transcript-list");
    const card = list.querySelector(`[data-card="${cssEscape(id)}"], [data-step="${cssEscape(id)}"]`);
    if (card) {
      const top = card.getBoundingClientRect().top - list.getBoundingClientRect().top + list.scrollTop;
      scrollTo(list, { top: Math.max(0, top - list.clientHeight / 2 + card.offsetHeight / 2), behavior });
    }
  }
  drawStatus(ctx);
  drawOverview(ctx);
  bindTranscript(ctx);
  bindTimeline(ctx);
  const teardownPanels = setupPanels(ctx);
  root.querySelectorAll("[data-tab]").forEach((b) => b.onclick = () => showTab(b.dataset.tab));
  ctx.q(".acv-dock-help-btn").onclick = () => {
    const box = ctx.q(".acv-dock-help");
    box.hidden = !box.hidden;
  };
  ctx.q(".acv-dock-help-close").onclick = () => {
    ctx.q(".acv-dock-help").hidden = true;
  };
  ctx.q(".acv-focus").onchange = (e) => {
    state.focus = e.target.checked;
    drawTimeline(ctx);
    drawTranscript(ctx);
  };
  ctx.q(".acv-zoom").oninput = (e) => {
    const v = Number(e.target.value);
    state.zoom = v / 100;
    ctx.q(".acv-zoom-out").textContent = `${v}%`;
    drawTimeline(ctx);
  };
  ctx.q(".acv-fit").onclick = () => centerOn(state.sel, "smooth");
  ctx.q(".acv-tl-back").onclick = () => {
    const b = ctx.q(".acv-tl-back");
    if (b.dataset.upStream && b.dataset.upStep) goToOpener(b.dataset.upStream, b.dataset.upStep, b.dataset.upTalk || null);
    else goBack();
  };
  ctx.q(".acv-talk-filter").oninput = () => drawTalkList(ctx);
  ctx.q(".acv-talk-list").addEventListener("click", (ev) => {
    const b = ev.target.closest("[data-talk-pick]");
    if (!b) return;
    openTalk(b.dataset.talkPick);
    setOverviewOpen(ctx, false);
  });
  const onPointerDown = (e) => {
    const t = e.target;
    if (state.overviewOpen && !(t && (t.closest(".acv-overview") || t.closest(".acv-overview-toggle")))) {
      setOverviewOpen(ctx, false);
    }
    const help = ctx.q(".acv-dock-help");
    if (!help.hidden && !(t && (t.closest(".acv-dock-help") || t.closest(".acv-dock-help-btn")))) help.hidden = true;
    const problems = ctx.q(".acv-problems");
    if (!problems.hidden && !(t && (t.closest(".acv-problems") || t.closest(".acv-integrity")))) setProblemsOpen(ctx, false);
  };
  document.addEventListener("pointerdown", onPointerDown);
  const onKey = (e) => {
    const active = document.activeElement;
    if (e.key === "Escape" && state.overviewOpen) {
      setOverviewOpen(ctx, false);
      return;
    }
    if (e.key === "Escape" && !ctx.q(".acv-dock-help").hidden) {
      ctx.q(".acv-dock-help").hidden = true;
      return;
    }
    if (e.key === "Escape" && !ctx.q(".acv-problems").hidden) {
      setProblemsOpen(ctx, false);
      return;
    }
    if (active && ["INPUT", "TEXTAREA", "SELECT"].includes(active.tagName)) return;
    if (active && active !== document.body && !root.contains(active)) return;
    if (e.key === "Escape") {
      clearSelection();
      return;
    }
    if (e.key === "Enter" && state.folder) {
      if (active && active.closest("button, a, select, [tabindex]") && active !== root) return;
      e.preventDefault();
      diveIn();
      return;
    }
    const k = e.key.toLowerCase();
    if (k !== "j" && k !== "k") return;
    if (!state.stream) return;
    const ev = model.steps(state.stream);
    const i = ev.findIndex((x) => x.id === state.sel);
    if (i < 0) return;
    const next = ev[k === "j" ? Math.min(ev.length - 1, i + 1) : Math.max(0, i - 1)];
    if (next) select(next.id, true);
  };
  document.addEventListener("keydown", onKey);
  function setState(pub) {
    muted++;
    try {
      applyState(pub);
    } finally {
      muted--;
    }
    emit();
  }
  function applyState(pub) {
    var _a, _b, _c, _d, _e, _f;
    const step = pub.step ? model.step(pub.step) : null;
    if (step) {
      const talk = step.talk ?? ((_a = model.talksOf(step.stream)[0]) == null ? void 0 : _a.id);
      if (talk && ((_b = state.talk) == null ? void 0 : _b.id) !== talk) openTalk(talk, true);
      else if (step.stream !== state.stream) switchStream(step.stream);
      select(step.id, true);
      return;
    }
    if (pub.talk && model.talkById.has(pub.talk)) {
      openTalk(pub.talk);
      if (pub.stream && pub.stream !== state.stream) switchStream(pub.stream);
      return;
    }
    if (pub.stream && model.streamByName.has(pub.stream)) {
      const first2 = model.talksOf(pub.stream)[0];
      if (first2) openTalk(first2.id);
      else {
        state.stream = pub.stream;
        state.sel = ((_c = model.steps(pub.stream)[0]) == null ? void 0 : _c.id) ?? null;
        renderAll(true);
      }
      return;
    }
    const first = model.firstTalk();
    if (first) openTalk(first.id);
    else {
      state.stream = ((_d = model.doc.streams.find((x) => x.role === "main")) == null ? void 0 : _d.name) ?? ((_e = model.doc.streams[0]) == null ? void 0 : _e.name) ?? null;
      state.sel = state.stream ? ((_f = model.steps(state.stream)[0]) == null ? void 0 : _f.id) ?? null : null;
      renderAll(true);
    }
  }
  setState(opts.state ?? {});
  return {
    destroy() {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onPointerDown);
      teardownPanels();
      root.innerHTML = "";
      root.classList.remove("acv");
    },
    setState,
    getState
  };
}
export {
  ASZ_VIEW_FORMAT,
  ASZ_VIEW_MAJOR_VERSION,
  ENGLISH,
  EN_US_FORMATTER,
  isSupportedDocument,
  makeFormatter,
  mountConversationView
};
//# sourceMappingURL=conversation-view.js.map
