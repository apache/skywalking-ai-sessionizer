// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// The forms a run can be written in.
const (
	formatText     = "text"
	formatMarkdown = "markdown"
	formatHTML     = "html"
)

// pageAssets are what a page draws with: Horizon's theme, and the logos and
// the icon the asz page shows.
type pageAssets struct {
	theme                   string
	logo, logoBlue, favicon []byte
}

// writeReport writes a run in one of the forms. A page is given what it draws
// with, and the conversation's steps by id, or why they could not be read,
// to show the steps its evidence rests on.
func writeReport(w io.Writer, doc reportDoc, format string, assets pageAssets, steps map[string]sessionview.Node, stepsErr error) error {
	switch format {
	case formatText:
		writeText(w, doc)
		return nil
	case formatMarkdown:
		writeMarkdown(w, doc)
		return nil
	case formatHTML:
		return writeHTML(w, doc, assets, steps, stepsErr)
	}
	return fmt.Errorf("-format %q: give text, markdown or html", format)
}

func (g goalSection) title() string {
	if g.statement == "" {
		return g.label
	}
	return g.label + ": " + g.statement
}

func textsOr(items []item, nothing string) []string {
	if len(items) == 0 {
		return []string{nothing}
	}
	var out []string
	for _, it := range items {
		out = append(out, it.text)
	}
	return out
}

func (t targetView) criteriaLines() []string {
	var out []string
	for _, c := range t.criteria {
		out = append(out, c.line())
	}
	return out
}

// branch is one line of the text report's tree: the numbered items under
// it, then the branches under it.
type branch struct {
	text  string
	items []string
	kids  []branch
}

func (t targetView) branch() branch {
	title := "target: " + t.statement
	if t.main {
		title += " (the main one)"
	}
	kids := []branch{{text: "status: " + t.status()}, {text: "score: " + t.score}}
	if len(t.criteria) > 0 {
		kids = append(kids, branch{text: "criteria", items: t.criteriaLines()})
	}
	kids = append(kids, branch{text: "done", items: textsOr(t.done, "nothing recorded")},
		branch{text: "missing", items: textsOr(t.missing, "nothing")})
	return branch{text: title, kids: kids}
}

// writeText writes a run as a tree, for a terminal.
func writeText(w io.Writer, doc reportDoc) {
	if doc.snapshot != "" {
		fmt.Fprintln(w, doc.snapshot)
	}
	fmt.Fprintf(w, "run %d of conversation %s\n", doc.run, doc.conversation)
	if !doc.closed {
		fmt.Fprintln(w, "\nthe run is not closed")
		return
	}
	for _, g := range doc.goals {
		fmt.Fprintf(w, "\n%s\n", g.title())
		var bs []branch
		if g.status != "" {
			bs = append(bs, branch{text: "status: " + g.status}, branch{text: "score: " + g.score})
		}
		for _, t := range g.targets {
			bs = append(bs, t.branch())
		}
		writeBranches(w, "", bs)
	}
	fmt.Fprintln(w, "\noverall assessment")
	writeItems(w, "", doc.assessment)
	fmt.Fprintln(w, "\noverall result")
	writeItems(w, "", doc.result)
}

// writeBranches writes branches under prefix, each drawn from the line
// above it.
func writeBranches(w io.Writer, prefix string, bs []branch) {
	for i, b := range bs {
		glyph, under := "├─ ", "│  "
		if i == len(bs)-1 {
			glyph, under = "└─ ", "   "
		}
		fmt.Fprintln(w, prefix+glyph+b.text)
		writeItems(w, prefix+under, b.items)
		writeBranches(w, prefix+under, b.kids)
	}
}

// writeItems writes items numbered; an item of several lines goes on under
// its number.
func writeItems(w io.Writer, prefix string, items []string) {
	for n, it := range items {
		number := fmt.Sprintf("%d. ", n+1)
		lines := strings.Split(it, "\n")
		fmt.Fprintln(w, prefix+number+lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintln(w, prefix+strings.Repeat(" ", len(number))+l)
		}
	}
}

// writeMarkdown writes a run's conclusions as Markdown, for a summary to
// share: a table of every goal's targets with how each ended and why, what
// each target is missing, then the overall assessment and result. The
// criteria, what was done and the evidence are left to the page.
func writeMarkdown(w io.Writer, doc reportDoc) {
	fmt.Fprintf(w, "# Run %d of conversation %s\n", doc.run, mdEscape(doc.conversation))
	if doc.snapshot != "" {
		fmt.Fprintf(w, "\nPulled from another asz as %s.\n", mdEscape(doc.snapshot))
	}
	if !doc.closed {
		fmt.Fprintln(w, "\nThe run is not closed.")
		return
	}
	fmt.Fprintln(w, "\n| Goal | Target | Status | Score |\n| --- | --- | --- | --- |")
	for _, g := range doc.goals {
		if g.status != "" {
			fmt.Fprintf(w, "| %s | no target | %s | %s |\n", mdCell(capital(g.title())), mdCell(g.status), mdCell(g.score))
		}
		for _, t := range g.targets {
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n", mdCell(capital(g.title())), mdCell(t.statement), mdCell(t.status()), mdCell(t.score))
		}
	}
	for _, g := range doc.goals {
		for _, t := range g.targets {
			if len(t.missing) > 0 {
				mdList(w, "Missing: "+t.statement, textsOr(t.missing, ""))
			}
		}
	}
	fmt.Fprintln(w, "\n## Overall assessment")
	fmt.Fprintln(w)
	mdItems(w, doc.assessment)
	fmt.Fprintln(w, "\n## Overall result")
	fmt.Fprintln(w)
	mdItems(w, doc.result)
}

func mdList(w io.Writer, title string, items []string) {
	fmt.Fprintf(w, "\n**%s**\n\n", mdEscape(title))
	mdItems(w, items)
}

// mdItems writes a numbered list; the lines of an item after its first break
// onto lines of their own, inside the item.
func mdItems(w io.Writer, items []string) {
	for i, it := range items {
		number := fmt.Sprintf("%d. ", i+1)
		lines := strings.Split(it, "\n")
		for j := range lines {
			lines[j] = mdEscape(lines[j])
		}
		fmt.Fprintln(w, number+strings.Join(lines, "\\\n"+strings.Repeat(" ", len(number))))
	}
}

// mdEscaper escapes the characters that would start emphasis, code, a link,
// a tag, a table cell or a character reference such as &copy;.
var mdEscaper = strings.NewReplacer(`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "<", `\<`, ">", `\>`, "|", `\|`, "&", `\&`)

// mdURL is a bare web address. GitHub's Markdown may link one by itself, and
// then keeps a backslash inside it as part of the address, so the escapes
// text needs would change it. Which addresses it links is the renderer's
// rule, so an address is written as code instead, which every renderer shows
// exactly as it stands, linked or not.
var mdURL = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>]+`)

// mdBlock is a line's start that Markdown reads as a block: a list item, a
// heading, or a numbered item; a quote's mark is escaped already.
var mdBlock = regexp.MustCompile(`^\s*([-+#]|\d+[.)])`)

// mdEscape writes a line so Markdown reads it as text.
func mdEscape(s string) string { return mdLine(s, false) }

// mdCell writes a table cell so Markdown reads it as text, on one line.
func mdCell(s string) string { return mdLine(strings.ReplaceAll(s, "\n", "; "), true) }

// mdLine escapes a line, its start included: a mark is escaped, and a
// number's dot or bracket. In a table, a cell separator is escaped in code
// too, since a table splits its cells before it reads code. An address that
// holds a backtick, which would end the code, or in a cell a backslash before
// a separator, which would leave the separator unescaped, is escaped as text:
// the table keeps its columns, though a renderer that links the address may
// show that backslash twice.
func mdLine(s string, cell bool) string {
	var b strings.Builder
	last := 0
	for _, m := range mdURL.FindAllStringIndex(s, -1) {
		url := s[m[0]:m[1]]
		if strings.Contains(url, "`") || cell && strings.Contains(url, `\|`) {
			continue
		}
		if cell {
			url = strings.ReplaceAll(url, "|", `\|`)
		}
		b.WriteString(mdEscaper.Replace(s[last:m[0]]) + "`" + url + "`")
		last = m[1]
	}
	b.WriteString(mdEscaper.Replace(s[last:]))
	s = b.String()
	if m := mdBlock.FindStringSubmatchIndex(s); m != nil {
		at := m[2]
		if m[3]-m[2] > 1 {
			at = m[3] - 1
		}
		s = s[:at] + `\` + s[at:]
	}
	return s
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// The page's data: what the template shows, each step its evidence rests on
// a link to that step, shown at the end of the page.
type pageData struct {
	Run          int
	Conversation string
	Snapshot     string
	Closed       bool
	Final        string
	FinalBand    string
	Tally        []pageChip
	Overview     []pageRow
	Goals        []pageGoal
	Assessment   []string
	Result       []string
	Steps        []pageStep
	StepsNote    string
	Theme, Style template.CSS
	// Logo, LogoBlue and Favicon are the asz page's images, held in the page.
	Logo, LogoBlue, Favicon template.URL
	Script                  template.JS
}

// pageStep is one step of the conversation, as the run read it: what it
// was, when, its text, and for a call what came back.
type pageStep struct {
	Anchor, What, When, State string
	Text, Result              string
	at                        int64
}

type pageChip struct{ Class, Text string }

// pageRow is one target in the table of the page's head: its goal, linked to
// the goal's card, and its details, linked to the target itself.
type pageRow struct{ GoalAnchor, Anchor, Goal, Target, Class, Words, Score, Band string }

type pageGoal struct {
	Anchor, Label, Statement, Status string
	Targets                          []pageTarget
}

type pageTarget struct {
	Anchor            string
	Statement         string
	Main              bool
	Class, Words, Why string
	WhyItem           pageItem
	Number, Band, Rec string
	Criteria          []pageCriterion
	Done, Missing     []pageItem
}

type pageCriterion struct {
	Text, State, StateClass, Reason string
	Against                         []pageItem
	Evidence                        []pageEvidence
}

type pageEvidence struct{ Excerpt, Reading, Link string }

type pageItem struct {
	Lines []string
	Links []string
}

// band is how a score is colored: whole, none, between, or not scored.
func band(v *float64) string {
	switch {
	case v == nil:
		return "none"
	case math.Round(*v*100) >= 100:
		return "ok"
	case math.Round(*v*100) <= 0:
		return "err"
	}
	return "warn"
}

func number(v *float64) string {
	if v == nil {
		return "–"
	}
	return fmt.Sprintf("%.0f", math.Round(*v*100))
}

// goalWords says in a few words what is known of one of the person's goals
// no target serves, for a badge.
var goalWords = map[string]string{sessionjudge.CompareAbsent: "not worked on", sessionjudge.CompareMissed: "no target found",
	sessionjudge.CompareUnknown: "unknown"}

func (doc reportDoc) page(assets pageAssets, nodes map[string]sessionview.Node, stepsErr error) pageData {
	var steps []pageStep
	anchors := map[string]string{}
	// link is where a step is shown on the page, the first time it is cited
	// added to the steps the page ends with; a step the conversation does
	// not hold, or one not read, has none.
	link := func(id string) string {
		if a, ok := anchors[id]; ok {
			return "#" + a
		}
		n, ok := nodes[id]
		if id == "" || !ok {
			return ""
		}
		a := fmt.Sprintf("step-%d", len(steps)+1)
		anchors[id] = a
		steps = append(steps, pageStep{Anchor: a, What: stepWords(n), When: when(n.At), State: stateWords(n), Text: n.Text, Result: n.Result, at: n.At})
		return "#" + a
	}
	links := func(steps []string) []string {
		var out []string
		for _, s := range steps {
			if l := link(s); l != "" && !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
		return out
	}
	items := func(list []item, nothing string) []pageItem {
		if len(list) == 0 && nothing != "" {
			return []pageItem{{Lines: []string{nothing}}}
		}
		var out []pageItem
		for _, it := range list {
			out = append(out, pageItem{Lines: strings.Split(it.text, "\n"), Links: links(it.steps)})
		}
		return out
	}
	d := pageData{Run: doc.run, Conversation: doc.conversation, Snapshot: doc.snapshot, Closed: doc.closed, Final: doc.final, FinalBand: band(doc.finalValue),
		Assessment: doc.assessment, Result: doc.result, Theme: template.CSS(assets.theme), Style: template.CSS(pageStyle),
		Logo: svgURL(assets.logo), LogoBlue: svgURL(assets.logoBlue), Favicon: svgURL(assets.favicon), Script: template.JS(pageScript)}
	for _, c := range doc.tally {
		d.Tally = append(d.Tally, pageChip{Class: c.class, Text: c.text})
	}
	for i, g := range doc.goals {
		pg := pageGoal{Anchor: fmt.Sprintf("goal-%d", i+1), Label: capital(g.label), Statement: g.statement, Status: g.status}
		if g.status != "" {
			d.Overview = append(d.Overview, pageRow{GoalAnchor: pg.Anchor, Anchor: pg.Anchor, Goal: pg.Label, Target: "no target", Class: "none",
				Words: goalWords[g.class], Score: "–", Band: "none"})
		}
		for j, t := range g.targets {
			pt := pageTarget{Anchor: fmt.Sprintf("%s-target-%d", pg.Anchor, j+1), Statement: t.statement, Main: t.main, Class: t.class, Words: t.words, Why: t.why, WhyItem: pageItem{Lines: []string{t.why}, Links: links(t.whySteps)}, Number: number(t.final),
				Band: band(t.final), Done: items(t.done, "nothing recorded"), Missing: items(t.missing, "nothing")}
			if t.record != nil && t.final != nil && number(t.record) != number(t.final) {
				pt.Rec = number(t.record) + " by the achievement record"
			}
			for _, c := range t.criteria {
				pc := pageCriterion{Text: c.text, State: c.state, StateClass: strings.ReplaceAll(c.state, " ", "_"), Reason: c.reason, Against: items(c.against, "")}
				for _, e := range c.evidence {
					pc.Evidence = append(pc.Evidence, pageEvidence{Excerpt: e.excerpt, Reading: e.reading, Link: link(e.step)})
				}
				pt.Criteria = append(pt.Criteria, pc)
			}
			pg.Targets = append(pg.Targets, pt)
			d.Overview = append(d.Overview, pageRow{GoalAnchor: pg.Anchor, Anchor: pt.Anchor, Goal: pg.Label, Target: t.statement, Class: t.class,
				Words: t.words, Score: pt.Number, Band: pt.Band})
		}
		d.Goals = append(d.Goals, pg)
	}
	// The steps read in the order they happened, whatever order cited them.
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at < steps[j].at })
	d.Steps = steps
	switch {
	case stepsErr != nil:
		d.StepsNote = "The steps the evidence rests on are not shown: " + stepsErr.Error()
	case nodes == nil:
		d.StepsNote = "The steps the evidence rests on are not shown for a conversation pulled from another asz."
	}
	return d
}

// stepWords says what a step was, for a person.
func stepWords(n sessionview.Node) string {
	switch n.Kind {
	case "message.external":
		return "the person's message"
	case "message.assistant":
		return "the agent's reply"
	case "message.synthetic":
		return "a message the runtime wrote"
	case "tool":
		return "tool " + n.Name
	case "agent":
		return "sub-agent " + n.Name
	case "llm.call":
		if n.Model != "" {
			return "model call to " + n.Model
		}
		return "model call"
	case "context.injection":
		return "context the runtime added"
	}
	return strings.ReplaceAll(n.Kind, ".", " ")
}

// stateWords says why a step shows no text, or less of it.
func stateWords(n sessionview.Node) string {
	switch n.State {
	case "", "available":
		return ""
	case "omitted":
		return "withheld"
	}
	return strings.ReplaceAll(n.State, "_", " ")
}

func when(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05 UTC")
}

// writeHTML writes a run as a page that opens as a file, with nothing to
// serve it: Horizon's design tokens and themes, a table of every goal's
// targets, then a card per goal whose targets show their score and status
// and open to their evidence, and the steps the evidence rests on at the end.
func writeHTML(w io.Writer, doc reportDoc, assets pageAssets, nodes map[string]sessionview.Node, stepsErr error) error {
	return pageTemplate.Execute(w, doc.page(assets, nodes, stepsErr))
}

// svgURL holds an image in the page, so it needs no file beside it. The
// images are asz's own, built into it.
func svgURL(svg []byte) template.URL {
	if len(svg) == 0 {
		return ""
	}
	return template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg))
}

var pageTemplate = template.Must(template.New("page").Funcs(template.FuncMap{"inc": func(i int) int { return i + 1 }}).Parse(pageHTML))

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Judge run {{.Run}} of {{.Conversation}} · SkyWalking AI Sessionizer</title>
{{if .Favicon}}<link rel="icon" href="{{.Favicon}}">{{end}}
<style>{{.Theme}}</style>
<style>{{.Style}}</style>
</head>
<body>
<header class="asz-header">
<span class="asz-brand" aria-label="SkyWalking AI Sessionizer">{{if .Logo}}<img class="logo-dark" src="{{.Logo}}" alt="SkyWalking">{{end}}{{if .LogoBlue}}<img class="logo-light" src="{{.LogoBlue}}" alt="SkyWalking">{{end}}<span>AI Sessionizer</span></span>
<span class="asz-title">Judge · run {{.Run}} · <span class="mono">{{.Conversation}}</span></span>
</header>
<main class="report">
<header class="head">
<div class="kicker">asz judge · run {{.Run}}</div>
<h1>{{.Conversation}}</h1>
{{if .Snapshot}}<div class="muted small">Pulled from another asz as <span class="mono">{{.Snapshot}}</span></div>{{end}}
{{if .Closed}}<div class="chips">{{if .Final}}<span class="score-chip band-{{.FinalBand}}">final {{.Final}}</span>{{end}}{{range .Tally}}<span class="badge class-{{.Class}}">{{.Text}}</span>{{end}}</div>{{end}}
</header>
{{if not .Closed}}<p class="muted">The run is not closed.</p>{{else}}
<section class="card">
<h2>Goals</h2>
<table class="overview">
<thead><tr><th>Goal</th><th>Target</th><th>Status</th><th class="num">Score</th><th></th></tr></thead>
<tbody>{{range .Overview}}<tr><td><a href="#{{.GoalAnchor}}">{{.Goal}}</a></td><td>{{.Target}}</td><td><span class="badge class-{{.Class}}">{{.Words}}</span></td><td class="num"><span class="score band-{{.Band}}">{{.Score}}</span></td><td class="num"><a class="button" href="#{{.Anchor}}">Check details</a></td></tr>{{end}}</tbody>
</table>
</section>
{{range .Goals}}<section class="card goal" id="{{.Anchor}}">
<div class="kicker">{{.Label}}</div>
<h2>{{.Statement}}</h2>
{{if .Status}}<div class="target-row"><span class="score band-none">–</span><div class="summary"><span><span class="badge">no target</span></span><span class="why">{{.Status}}</span></div></div>{{end}}
{{range .Targets}}<details class="target" id="{{.Anchor}}">
<summary><span class="score band-{{.Band}}">{{.Number}}</span><span class="summary"><span><span class="badge class-{{.Class}}">{{.Words}}</span>{{if .Main}} <span class="badge">main</span>{{end}}</span><span class="title">{{.Statement}}</span>{{if .Why}}<span class="why">{{template "item" .WhyItem}}</span>{{end}}{{if .Rec}}<span class="muted small">{{.Rec}}</span>{{end}}<span><span class="button show-details">Check details</span><span class="button hide-details">Hide details</span></span></span></summary>
<div class="panels">
{{if .Criteria}}<div class="panel"><div class="kicker">Criteria</div><ol>{{range .Criteria}}<li><span class="crit">{{.Text}}</span> <span class="badge state-{{.StateClass}}">{{.State}}</span>{{range .Against}}<div class="against">{{template "item" .}}</div>{{end}}{{if .Reason}}<div class="reason">{{.Reason}}</div>{{end}}{{range .Evidence}}<div class="evidence">{{if .Excerpt}}<code>{{.Excerpt}}</code>{{end}}{{if .Reading}}<span>{{.Reading}}</span>{{end}}{{if .Link}} <a class="step" href="{{.Link}}">the step</a>{{end}}</div>{{end}}</li>{{end}}</ol></div>{{end}}
<div class="panel"><div class="kicker">Done</div><ol>{{range .Done}}<li>{{template "item" .}}</li>{{end}}</ol></div>
<div class="panel"><div class="kicker">Missing</div><ol>{{range .Missing}}<li>{{template "item" .}}</li>{{end}}</ol></div>
</div>
</details>
{{end}}</section>
{{end}}<section class="card"><h2>Overall assessment</h2><ol>{{range .Assessment}}<li>{{.}}</li>{{end}}</ol></section>
<section class="card"><h2>Overall result</h2><ol>{{range .Result}}<li>{{.}}</li>{{end}}</ol></section>
{{if .Steps}}<details class="card steps" id="steps"><summary><h2>Steps</h2><span class="muted small">{{len .Steps}} steps of the conversation the evidence above rests on, as the run read them; a link to one opens it</span></summary>
{{range .Steps}}<div class="step-card" id="{{.Anchor}}"><div class="step-head"><span class="badge">{{.What}}</span>{{if .When}} <span class="muted small">{{.When}}</span>{{end}}{{if .State}} <span class="badge">{{.State}}</span>{{end}}</div>{{if .Text}}<pre>{{.Text}}</pre>{{end}}{{if .Result}}<div class="kicker">What came back</div><pre>{{.Result}}</pre>{{end}}</div>
{{end}}</details>{{end}}{{if .StepsNote}}<p class="muted small">{{.StepsNote}}</p>{{end}}
{{end}}</main>
{{if .Steps}}<dialog class="step-dialog" id="step-dialog" aria-label="A step of the conversation"><div class="step-dialog-head"><span class="kicker">A step of the conversation</span><form method="dialog"><button class="button">Close</button></form></div><div class="step-dialog-body" id="step-dialog-body"></div></dialog>{{end}}
<script>{{.Script}}</script>
</body>
</html>
{{define "item"}}{{range $i, $l := .Lines}}{{if $i}}<br><span class="muted">{{$l}}</span>{{else}}{{$l}}{{end}}{{end}}{{if eq (len .Links) 1}} <a class="step" href="{{index .Links 0}}">the step</a>{{else}}{{range $i, $u := .Links}} <a class="step" href="{{$u}}">step {{inc $i}}</a>{{end}}{{end}}{{end}}`

// pageScript opens a target named in the address, so a link to its details
// shows them even when the reader closed them since, and shows a cited step in a
// dialog rather than sending the reader to the end of the page. The dialog holds
// a copy of a step the template wrote, so nothing is fetched.
const pageScript = `
(function () {
  function open(id) {
    var el = id && document.getElementById(id);
    if (!el) return;
    var d = el.tagName === "DETAILS" ? el : el.closest("details");
    if (d) d.open = true;
  }
  window.addEventListener("hashchange", function () { open(location.hash.slice(1)); });
  open(location.hash.slice(1));
  var dialog = document.getElementById("step-dialog"), body = document.getElementById("step-dialog-body");
  var modal = dialog && typeof dialog.showModal === "function";
  document.addEventListener("click", function (e) {
    var a = e.target.closest && e.target.closest('a[href^="#"]');
    if (!a) return;
    var id = a.getAttribute("href").slice(1), card = document.getElementById(id);
    if (modal && card && a.classList.contains("step")) {
      e.preventDefault();
      var copy = card.cloneNode(true);
      copy.removeAttribute("id");
      body.replaceChildren(copy);
      dialog.showModal();
      return;
    }
    open(id);
  });
  if (modal) dialog.addEventListener("click", function (e) { if (e.target === dialog) dialog.close(); });
})();
`

// pageStyle draws the report with Horizon's tokens, so it reads as the
// SkyWalking UI does, in whichever of its themes.
const pageStyle = `
.asz-header { display: flex; align-items: center; gap: 14px; padding: 7px 14px; border-bottom: 1px solid var(--sw-line); background: var(--sw-bg-1); }
.asz-brand { display: flex; align-items: center; gap: 8px; color: var(--sw-fg-0); font-weight: var(--sw-fw-semibold); white-space: nowrap; }
.asz-brand img { height: 22px; }
.logo-light { display: none; }
html[data-appearance="light"] .logo-dark { display: none; }
html[data-appearance="light"] .logo-light { display: block; }
.asz-title { min-width: 0; overflow: hidden; color: var(--sw-fg-1); font-size: var(--sw-fs-sm); text-overflow: ellipsis; white-space: nowrap; }
.mono { font-family: var(--sw-mono); }
body { margin: 0; background: var(--sw-bg-0); color: var(--sw-fg-0); font-family: var(--sw-font, system-ui, sans-serif);
  font-size: var(--sw-fs-base, 13px); line-height: var(--sw-lh-normal, 1.5); }
.report { max-width: 1200px; margin: 0 auto; padding: 24px 16px 64px; }
.kicker { color: var(--sw-fg-2); font-size: var(--sw-fs-xs); letter-spacing: var(--sw-ls-caps); text-transform: uppercase; }
.head h1 { margin: 6px 0 0; font-family: var(--sw-mono); font-size: var(--sw-fs-xl); font-weight: var(--sw-fw-semibold); overflow-wrap: anywhere; }
h2 { margin: 6px 0 14px; font-size: var(--sw-fs-lg); font-weight: var(--sw-fw-semibold); }
a { color: var(--sw-accent); text-decoration: none; }
a:hover { text-decoration: underline; }
.chips { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 14px; }
.card { margin: 16px 0; padding: 18px; border: 1px solid var(--sw-line); border-radius: var(--sw-radius); background: var(--sw-bg-1); }
.badge { display: inline-flex; align-items: center; padding: 2px 9px; border: 1px solid var(--sw-line); border-radius: 999px;
  background: var(--sw-bg-3); color: var(--sw-fg-1); font-size: var(--sw-fs-xs); white-space: nowrap; }
.class-achieved, .state-met { color: var(--sw-ok); background: var(--sw-ok-soft); border-color: transparent; }
.class-partly, .class-reverted, .state-unknown { color: var(--sw-warn); background: var(--sw-warn-soft); border-color: transparent; }
.class-not_achieved, .class-contradicted, .state-not_met, .state-contradicted { color: var(--sw-err); background: var(--sw-err-soft); border-color: transparent; }
.score { display: inline-block; min-width: 2.6em; font-size: var(--sw-fs-2xl); font-weight: var(--sw-fw-bold); font-variant-numeric: tabular-nums; text-align: right; }
.overview .score { min-width: 0; font-size: var(--sw-fs-md); }
.band-ok { color: var(--sw-ok); }
.band-warn { color: var(--sw-warn); }
.band-err { color: var(--sw-err); }
.band-none { color: var(--sw-fg-3); }
.score-chip { padding: 2px 10px; border: 1px solid currentColor; border-radius: 999px; font-size: var(--sw-fs-sm); font-weight: var(--sw-fw-semibold); }
table.overview { width: 100%; border-collapse: collapse; }
.overview th { padding: 8px; border-bottom: 1px solid var(--sw-line); color: var(--sw-fg-2); font-size: var(--sw-fs-xs); font-weight: var(--sw-fw-medium);
  letter-spacing: var(--sw-ls-caps); text-align: left; text-transform: uppercase; }
.overview td { padding: 10px 8px; border-bottom: 1px solid var(--sw-line); vertical-align: top; }
.overview .num { text-align: right; }
details.target { margin-top: 12px; border: 1px solid var(--sw-line); border-radius: var(--sw-radius); background: var(--sw-bg-2); }
details.target > summary { display: flex; gap: 16px; align-items: flex-start; padding: 12px 14px; cursor: pointer; list-style: none; }
details.target > summary::-webkit-details-marker { display: none; }
details.target[open] > summary { border-bottom: 1px solid var(--sw-line); }
.target-row { display: flex; gap: 16px; align-items: flex-start; margin-top: 12px; padding: 12px 14px; border: 1px dashed var(--sw-line); border-radius: var(--sw-radius); }
.summary { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.title { font-weight: var(--sw-fw-semibold); }
.why { color: var(--sw-fg-1); }
.muted { color: var(--sw-fg-2); }
.small { font-size: var(--sw-fs-xs); }
.panels { display: grid; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); gap: 12px; padding: 14px; }
.panel { min-width: 0; padding: 12px; border: 1px solid var(--sw-line); border-radius: var(--sw-radius); background: var(--sw-bg-1); }
ol { margin: 8px 0 0; padding-left: 22px; }
li { margin: 6px 0; overflow-wrap: anywhere; }
.reason { margin-top: 4px; color: var(--sw-fg-1); }
.against { margin-top: 4px; color: var(--sw-err); }
.evidence { margin-top: 6px; padding: 6px 8px; border-left: 3px solid var(--sw-info); background: var(--sw-bg-3); font-size: var(--sw-fs-sm); }
.evidence code { display: block; margin-bottom: 2px; color: var(--sw-fg-0); font-family: var(--sw-mono); white-space: pre-wrap; }
a.step { font-size: var(--sw-fs-xs); white-space: nowrap; }
.button { display: inline-flex; align-items: center; padding: 3px 10px; border: 1px solid var(--sw-accent-line, var(--sw-line)); border-radius: 999px;
  background: var(--sw-accent-soft, var(--sw-bg-3)); color: var(--sw-accent); font: inherit; font-size: var(--sw-fs-xs); font-weight: var(--sw-fw-medium);
  white-space: nowrap; cursor: pointer; text-decoration: none; }
a.button:hover, .button:hover { text-decoration: none; filter: brightness(1.15); }
details.target[open] .show-details, details.target:not([open]) .hide-details { display: none; }
details.steps > summary { display: flex; flex-wrap: wrap; gap: 12px; align-items: baseline; cursor: pointer; list-style: none; }
details.steps > summary h2 { margin: 0; }
.step-dialog { width: min(920px, 92vw); max-height: 86vh; padding: 0; border: 1px solid var(--sw-line); border-radius: var(--sw-radius);
  background: var(--sw-bg-1); color: var(--sw-fg-0); }
.step-dialog::backdrop { background: rgba(0, 0, 0, 0.55); }
.step-dialog-head { display: flex; align-items: center; justify-content: space-between; padding: 10px 14px; border-bottom: 1px solid var(--sw-line); }
.step-dialog-head form { margin: 0; }
html { scrollbar-gutter: stable; }
/* The page behind an open step does not scroll, or the reader loses their place. A rule on the
   dialog's own state, not a class the script sets and clears, since its close event can come late. */
html:has(dialog[open]) { overflow: hidden; }
.step-dialog-body { max-height: calc(86vh - 52px); overflow: auto; overscroll-behavior: contain; padding: 14px; }
.step-dialog-body .step-card { margin: 0; padding: 0; border: 0; background: transparent; }
.step-dialog-body pre { max-height: none; }
.step-card { margin-top: 12px; padding: 12px; border: 1px solid var(--sw-line); border-radius: var(--sw-radius); background: var(--sw-bg-2); scroll-margin-top: 16px; }
.step-card:target { outline: 2px solid var(--sw-accent); outline-offset: 2px; }
.step-head { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-bottom: 8px; }
pre { margin: 6px 0 0; padding: 8px 10px; max-height: 320px; overflow: auto; border-radius: var(--sw-radius); background: var(--sw-bg-3);
  color: var(--sw-fg-0); font-family: var(--sw-mono); font-size: var(--sw-fs-sm); white-space: pre-wrap; overflow-wrap: anywhere; }
@media (max-width: 640px) { details.target > summary, .target-row { flex-direction: column; gap: 6px; } .score { text-align: left; } }
`
