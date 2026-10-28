# Design panel

Part of the [AgentK design](../design.md).

During Design the panel opens as soon as the design exists and again whenever it's revised, at
about 60% of the work area. An Artifact design is shown in a sandboxed frame (scripts allowed, no
same-origin access, and a content policy that blocks network requests and limits scripts, styles
and fonts to what Artifacts use, placed first in the frame's document so nothing of the page runs
before it), so the page can't read Meko's session cookie or call the API. **Open on claude.ai** links to
the published Artifact. A markdown design is rendered as sanitized HTML with no forms or styles,
and its links open in a new tab. Its ` ```mermaid ` blocks are drawn as diagrams (the Design prompt
tells Claude they are): Mermaid is loaded only when a design has one and draws with SVG text in a
system font. What keeps a diagram out of the app is Mermaid's strict mode: labels are sanitized (or
drawn as SVG text) and click callbacks are off. Mermaid draws in the app's own page, in a container
it adds and removes, so while it draws, a label that got past its sanitizing would be live in the
app, and images a diagram names are fetched. The finished diagram is shown as an `<img>` of its SVG,
never inline, so what's on screen can't run, load anything or restyle the app. Markdown is
sanitized with its own DOMPurify instance, out of reach of the hooks Mermaid adds to the shared one.
A diagram fits the column, a tap opens it full size, and its source is a tap away. The full-size
view starts at 100% in a dialog sized to the diagram, up to the window (a full-width sheet on a
phone), and scrolls past that. With a mouse, **−** and **+** zoom it, in the column or full size,
where the dialog grows and shrinks with it (the size shown between them fits it back to the
width; phones pinch instead). A diagram that doesn't parse shows Mermaid's error above its
source, asking you to have AgentK fix it. An Artifact page with
`class="mermaid"` blocks that leaves drawing them to the claude.ai viewer gets the same Mermaid
version and look from jsdelivr, which the frame's policy already allows, so its diagrams show here
too. They're drawn one at a time, and a block that doesn't draw is replaced by the same note,
Mermaid's error and the source Mermaid read, as text, instead of Mermaid's error graphic. The usual
cause is `<` directly before a letter, `/`, `!` or `?` in a label (`<session>`, `List<T>`): the
browser parses the page first, so it becomes an HTML tag before Mermaid reads the block, here and
in the claude.ai viewer alike. Any other `<` or `>`, such as the arrows `->>` and `<-->`, stays
text. The Design prompt tells Claude not to write tag-like text in an Artifact's Mermaid blocks;
markdown fences are taken out as text before any HTML parsing, so they're unaffected.

**Open questions and key design decisions.** Besides the design, AgentK gives you its open questions in
`<home>/designs/<session>/questions.json` (`{"questions": [{"id", "kind", "question", "context",
"options": [{"label", "detail"}], "recommended", "status": "open" | "resolved", "decision", "decidedBy",
"meko"}]}`): through the [mod's `design_questions` tool](mod.md) when the session's agent has it, which saves
the file and shows them at once, or else by writing the file, read at the turn's end (Kiro, which
has no mod, always writes the file).
The line asking for them is added to the Design prompt in code (`designQuestionsLine()`, which names
the tool only when the session's agent gets the mod as far as is known when the prompt is written:
a harness that runs it, `[claude] mod` on, its files there, and a Claude Code not known to be too old), so a repo's own `design` prompt gets it too.

The same list holds the design's **Key Design Decisions** (KDDs): the questions, and the decisions AgentK made
itself that set the design's direction (`"kind": "decision"`: the decision point, the decision, why
in `context`, and the alternatives it weighed as `options`, its pick as `recommended`). A decision
is always resolved, and one without its decision is dropped. `decidedBy` says who settled each
entry: `user` for an answer or a change to AgentK's decision, `agentk` for AgentK's own decision or a
question left to it, null while open (and in files from before). The server sets it when it records
answers; AgentK sets it only on its own decisions, and is asked to keep it as written. When AgentK
rewrites the file (or calls the tool) leaving it out, a resolved entry keeps what the questions kept
before said (`parseDesignQuestions(text, prev)`), so a revision never turns the user's answer into
nobody's, or a decision the user changed back into AgentK's. `meko` lists up to 5 Meko items a decision (or a
recommendation) rests on, as AgentK cites them: `{"kind": "memory" | "knowledge", "id", "datapackId",
"excerpt"}`, with the id Meko gave it (from its own searches, or the ids in the
[recall block](recall-and-learn.md#meko-recall-at-start)) and the words it relied on, cut to 500
characters. Nothing is read from Meko to show them: the excerpt is the record, and the id is what
to look the item up by later. Questions are numbered Q1… and decisions D1…, each in the order
AgentK listed them (`decisionNumbers()` in `src/shared/decisions.ts`), for the user and for AgentK. The server keeps each session's questions in memory, never in
the database (`DesignQuestionStore` in `answers.ts`): they're read the first time they're asked
for, for every session in Design at startup (so the first turn end after a restart can tell
whether they changed), and again at each Design turn end. A read is capped (256 KB, 30 questions
and 30 decisions, 6 options and 5 Meko items each, text cut short) and normalized (ids lowercased to
letters, digits and dashes; entries without an id or question, and repeated ids, dropped; a
recommendation must point at an option; a Meko item needs a kind it knows and an id of letters,
digits, dashes and underscores). A file that's too large, can't be read or isn't questions is logged and leaves the ones
kept alone. That holds within one server run: after a restart there's nothing kept, so a broken file
shows the markdown design's list or no questions, and the session's activity log says the file
needs fixing. The `designs` questions columns and the `design_answers` table from before are left
in place, unused, so the release before this one still works after a rollback; a later migration
drops them.
Without the file, a markdown design's top-level list items under an "Open questions" heading are
its questions (only those with a "?" under a heading that also names risks), with ids from their
text. A change to the questions counts as a change to the design: the design's time moves on so
the panel refreshes, and the session is *Design ready* again.

The panel then has two tabs, **Design** and **Questions** (with the open count, also shown on the
Design doc button and the ⋯ sheet's row); on the Design tab a bar says how many questions are open
and how many decisions AgentK made (and how many of those from Meko), and jumps to them; with only
decisions, in Design, it's in the accent color rather than the warning one. The full page on a wide screen shows the questions in a column beside the design
instead. Each open question is a card: the question, its context, the options as radio rows with
AgentK's recommendation marked and picked to start with, **Custom answer**, and one text box: a
note on the option picked (optional), or the answer itself with Custom answer (or when there are no
options). Resolved questions and AgentK's decisions are listed under **Key Design Decisions** as read-only
cards in the file's order: the decision, who decided it (*You decided*, *AgentK decided*, *Left to
AgentK*, or *Resolved* for a file from before that doesn't say), for AgentK's own why and what else
it weighed, and the Meko items. In Design the section starts collapsed, so the open questions and
the feedback come first; it opens by itself while one of AgentK's decisions is being changed, again
whenever Approve sends you there to finish that change (even after you collapsed it), and when
**Review ›** on the Design tab's decisions bar (shown with no open question left) asks to see the
decisions. Past Design the decisions are all the tab has to show, so it starts open. A decision or question that rests on Meko carries Meko's "m"
(`public/meko-m.png`, drawn as a mask in the badge's color, `.meko-mark`) in a round badge, and each
Meko item shows the excerpt, its kind, its datapack (by name when it's one of the project's), its id
and **Copy id** (`MekoRefs.tsx`). In Design, **Change this** on one of AgentK's decisions turns it into
an answer card (its alternatives with the option in force selected, which is AgentK's pick unless
the user changed it before, or their own words, **Custom answer**, a note) with **Keep AgentK's
decision** to drop the change; the change goes with the answers, and only when it changes
something (another option, a note, or the user's own words). Past Design, an open question shows nothing
picked and reads *Unanswered*, never the recommendation. **Other feedback**
takes anything else about the design. Question text, decisions and Meko excerpts render as plain
text, never HTML.

Drafts aren't stored on the server. The browser keeps them per session in localStorage
(`ak-design-answers:<session>`, `src/web/design-answers.ts`), in step across tabs, so a reload or
the full page keeps them; their text boxes are reload-safe. An option is kept by label too: when
a revision moves it, the pick follows it; when it's gone, the question starts over at the
recommendation, keeping the text. A change to one of AgentK's decisions keeps the decision it was
begun against (`against`), and is dropped once the decision is no longer that: sent and recorded,
or revised by AgentK.

The approval goes from the panel, never from the stage bar. The stage bar's gate (and the ⋯
sheet's) is **Design Review** (`designGate()`; disabled before there's a design, and, like every
gate, while the session is busy or another action from the view is out): it opens the
panel on the questions while any are open, or while a change to one of AgentK's decisions waits in
this browser (`changingDecisions()`, which `approveDesign()` refuses, since its request can't carry
it), and on the design otherwise; with the panel already open it brings the panel's approve button
into view and focuses it (`reviewTarget()`, the panel's `approveAt`): the questions' footer
**Approve with answers** (**Cancel approval** while one waits, or the footer itself while the
button is disabled: `reviewFocus()`; its line gives Approve's reason before Send for revision's)
while they're on screen, else the header's **Approve design** (or, while it's disabled, the line
under the header that says why: busy, or `designApprovalBlocker()`, shown whenever the header's
Approve is closed, as a phone has no tooltips), or, with open questions on the Design tab, the
questions. Only a bump made while the panel is up counts, never one it mounted with from another
session's panel. A gate that only showed the questions did nothing when they were already on
screen, which on a phone they usually are. With open questions the header's **Approve design** reads **Answer & approve** and takes you to
them, and the approval goes from their footer, **Approve with answers** (the header leaves its
button out while the questions are on screen, or can't be shown because the design couldn't be
read, and does the same, reading **Answer & approve**, while a change to one of AgentK's decisions
waits: `headerApproves(session, questions, changing)`). On a panel too
short for the questions and their footer (a phone with the keyboard up), the view scrolls, so the
footer stays reachable. `Ctrl+Enter` (`⌘Enter`) in an answer, a note or the feedback presses
**Approve with answers** (**Approve design** when there are no open questions), as a click would: nothing while
it's disabled or while an approval waits for the turn's end; in **AgentK is asking** it presses that
form's **Answer** instead (`questionsSubmitButton()` in `DesignQuestions.tsx`). The answers go in the approval request (`DesignAnswersRequest`), sent ahead of the Code
prompt as `approved-answers`, every open question listed: answered, or left to AgentK's
recommendation (its own judgment when it has none). **Send for revision** sends them as the `design-answers` prompt instead, and AgentK
revises the design. Without AgentK's mod it only works
while Claude is at its prompt (between turns, or with the design waiting for approval, never over a
permission or question prompt); otherwise the button says why, and nothing is queued. With the mod
both work mid-turn: see [Answers and approvals mid-turn](mod.md). The server checks the
answers against the current questions: one to a question no longer open (or a decision that's
gone), or whose option's label changed, or a change to a decision AgentK has revised since the user
saw it (the request carries the decision it was made against, `DesignAnswerInput.decision`), is
refused (409) so the user looks again; answers too long for one prompt (50,000
characters) are refused too.

Once the answers have gone to Claude, the server records them in `questions.json`
(`DesignQuestionStore.record`): each answered question is resolved, with the answer as its decision
(the option's label, then ` — ` and the note if there is one, or the custom answer) and `decidedBy:
"user"`, and so is each of AgentK's decisions the user changed. An approval
also resolves every other open question, as *Left to AgentK: <recommended option>* (or *Left to
AgentK* without one), with `decidedBy: "agentk"`. The questions are read from the file as it is then, so a change AgentK made
mid-turn is kept, and an answer to a question that changed since the answers were checked is left
out; a file that can't be read is left alone for AgentK to fix (logged), never overwritten from
the questions kept in memory. The file is written the same way as the tool's (temp file, then
rename), the kept questions are replaced, and the design's time moves on so every browser shows
them; this isn't a change AgentK made in its turn, so it neither drops a queued approval nor marks
the design ready. The `design-answers` prompt still asks Claude to mark each answered question
resolved, saying AgentK may have done so already, and to follow the user's change to a
decision (listed as `### D<n>` with *You decided* and *The user wants*), keeping it a decision
decided by the user. After the approval, the design's **Key Design Decisions** section, near its top right after the
goal, is brought up to date, or added if it's missing (`designQaLine()`): every question and decision, who decided it, what and why,
and its Meko items by kind, datapack and id; the Design prompt already asks for AgentK's own
decisions there, so a reviewer reads the design's direction first; `designUpkeepLine()` adds decisions made while coding there (they don't
reach the panel). All three ask for Meko's marker, `<span class="meko-source">from Meko</span>`
(`MEKO_SOURCE_MARK` in `src/shared/decisions.ts`), after the title of each decision in that section
that rests on Meko, and only those. The panel draws it as Meko's "m" badge with CSS alone, its text
kept for screen readers: a `.design-body .prose .meko-source` rule for a markdown design (the
sanitizer keeps the class; the rule is scoped to the design, as held reviews render markdown too
and are other people's text), and for an Artifact a style `designFrameDoc()` adds after the page when it names the
class (`mekoSourceStyle()`, the mark as a `data:` URL the frame's policy allows, `!important` so the
page's own styles don't undo it). Anywhere else (claude.ai, GitHub, Meko) it reads as plain "from
Meko". The badge is only as right as AgentK's marking; the Questions tab's comes from the questions
file. A PR design written before calls the section "Questions and decisions", which the
Review a PR prompts still name. A markdown design's questions are only
written with the approval: until then AgentK revises its "Open questions" list, which a questions
file would hide. Nothing is recorded when the send fails.

What an approval did beyond the user's answers is noted in `questions-approval.json` beside the
questions file: the questions it left to AgentK, and whether it made the file (for a markdown
design). When the session is moved back to Design, `reopen` undoes that: those questions are open
again (`decidedBy` back to null), and a file it made is removed so the design's own list shows again.

After an approval goes out, the browser forgets the drafts (an approval queued for the turn's end
keeps them, in case it's dropped). After a revision it forgets the feedback and keeps the answers
until their questions are no longer open, so they still show until the questions come back
resolved, and for a markdown design's questions until AgentK changes them. Answers reach
Meko in the revised design and the session's conversation.

While AgentK works on the design, the panel's header shows it: a bar runs along the header's
bottom edge, and the header's "updated" time reads **Revising** (after **Send for revision** from
this page) or **Working** (any other turn), with the keycaps. `designWorking()` in `src/web/util.ts`
decides it from the session's status, so a reload or another screen shows it too: a session in
Design with a design on screen and AgentK working (or Claude holding answers for its next turn,
through the mod), never while Claude waits on the user. A Send for revision from this page
(`revisionSent()` in `src/web/design-answers.ts`, kept for this page only) is timed from the click
and covers the 10 seconds (`REVISION_GRACE_MS`) before Claude reports the turn. `settleRevision()`
follows it on every state push (`settleRevisions()`, from `App.tsx`, so it doesn't need the panel
open): it's marked started once its turn is seen within those 10 seconds, and forgotten once that
turn is over and Claude holds no answers, or when its turn was never seen in time, so a later turn
isn't taken for it. The footer's "Sent" line goes with it. A design written while the bar showed,
or just after it stopped (`designLanding()`), makes the "updated just now" time flash once as it
comes back (on a phone with tabs, where the time is hidden, it shows for those few seconds). With
reduced motion the bar stands still and nothing flashes.
