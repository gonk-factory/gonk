package beadstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BdCLI shells the MIT `bd` binary against the shared Dolt bead store (spec
// 4.2) instead of holding state in memory. It stores a Record as a
// structured comment on the bead -- `<!-- gonk-state {json} -->`, one per
// write, resolved on read by resolveComments (the latest unconditional write,
// or a later conditional one whose precondition held) -- plus a
// `gonk::<state>` label so a human can see a bead's gate state in the beads UI
// without decoding the comment, and so List can find it. The schema is
// documented in docs/reference/gonk-state-record.md.
//
// *** THE EXACT SUBCOMMANDS BELOW ARE CONFIRMED AGAINST THE REAL `bd` 1.0.3
// BINARY *** (the version pinned as BD_VERSION in images/versions.env). Three invocations an
// earlier draft of this file guessed at were WRONG, confirmed by actually
// running them:
//
//  1. `bd label <id> remove/add <label>` (subcommand AFTER the id) is not
//     valid usage -- cobra treats `<id>` as an unrecognized `label`
//     subcommand, PRINTS HELP TO STDOUT, AND EXITS 0. That means Put's
//     label update silently no-oped while reporting success: the
//     `gonk::<state>` label -- the one thing meant to let a human see a
//     bead's gate state without decoding the comment -- would never have
//     been set, in production, with no error anywhere. The real subcommand
//     order is `bd label remove/add <id> <label>` (verb immediately after
//     `label`).
//  2. `bd list`/`bd comments` take the documented global boolean `--json`
//     flag, not `--format json` (`--format` is a distinct, separate flag on
//     `bd list` for `digraph`/`dot`/a Go template; relying on it happening
//     to also accept the literal string "json" is not a documented
//     contract worth depending on).
//
// Nothing in cmd/gonk-gate's tested decision logic depends on this file:
// every test in that tree runs against Memory (AD-2). This file's own
// TestBdCLIInvokesTheRealSubcommandShapes (bd_test.go) pins the corrected
// argv shapes with a fake Run, so a future edit here cannot silently
// reintroduce the label bug above.
type BdCLI struct {
	// Bin is the `bd` executable. Defaults to "bd" (PATH lookup) when empty.
	Bin string
	// Dir is the working directory `bd` runs in (the checkout holding the
	// shared Dolt bead store). Empty means the current process's directory.
	Dir string
	// Run executes one `bd` invocation and returns its captured stdout.
	// Overridable so a future test can fake the binary without a real `bd`
	// on PATH; production code leaves it nil and gets execRun.
	Run func(ctx context.Context, dir, bin string, args ...string) ([]byte, error)
}

const gonkStateMarkerPrefix = "<!-- gonk-state "
const gonkStateMarkerSuffix = " -->"
const gonkLabelPrefix = "gonk::"

func (b *BdCLI) bin() string {
	if b.Bin == "" {
		return "bd"
	}
	return b.Bin
}

func (b *BdCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	if b.Run != nil {
		return b.Run(ctx, b.Dir, b.bin(), args...)
	}
	return execRun(ctx, b.Dir, b.bin(), args...)
}

func execRun(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("beadstore: %s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

// findBeadID resolves a BeadAnchor to bd's own bead id via a dedicated
// anchor label -- the only way to look a bead up by our idempotency key
// without re-deriving it from the GitLab artifact every time.
func (b *BdCLI) findBeadID(ctx context.Context, anchor string) (string, bool, error) {
	out, err := b.run(ctx, "list", "--label", anchorLabel(anchor), "--json")
	if err != nil {
		return "", false, err
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil {
		return "", false, fmt.Errorf("beadstore: decode bd list: %w", err)
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0].ID, true, nil
}

const anchorLabelPrefix = "gonk-anchor:"

func anchorLabel(anchor string) string { return anchorLabelPrefix + anchor }

// stateComment is the JSON inside one `<!-- gonk-state {json} -->` comment:
// the Record's fields, flat, plus IfRevision on a conditional write.
//
// IfRevision is what lets a conditional write be decided BY THE LOG rather
// than by the writer's earlier read (see resolveComments). It is a pointer so
// "unconditional" (absent) and "conditional on the empty revision" (present,
// "") stay distinct.
type stateComment struct {
	Record
	IfRevision *string `json:",omitempty"`
}

// resolveComments folds a bead's comments, in the order bd returns them, into
// the current record. It is THE read rule for BdCLI, and every read goes
// through it:
//
//   - an unconditional gonk-state comment (Put) always becomes current;
//   - a conditional one (PutIf) becomes current only if its IfRevision equals
//     the Revision of the record current at its position in the log. One that
//     does not is a write that lost its race, and it never takes effect.
//
// WHY THE LOG DECIDES AND NOT THE WRITER. BdCLI's writes are several separate
// bd processes, so a writer that checked the current revision can be overtaken
// before its comment lands. If it then simply appended, its stale copy would
// become "the last comment" and win -- the exact overwrite that erased MPKO's
// reservation in gonk-p8w8. Deciding at read time means the loser is ignored
// by every reader whatever order the two inserts commit in: bd sorts comments
// by (created_at, id), a client-side timestamp, which is not commit order, and
// this rule does not need it to be.
//
// If watch is non-empty, watched reports whether the comment carrying that
// Revision took effect -- how PutIf learns whether it won.
//
// A comment that does not decode is skipped if a later one decodes, and is an
// error if it is the last, which is what "last comment wins" did before.
func resolveComments(texts []string, watch string) (cur Record, found, watched bool, err error) {
	cur, _, found, watched, err = resolveCommentsAt(texts, watch)
	return cur, found, watched, err
}

// resolveCommentsAt is resolveComments that also returns the index in texts of
// the comment that became current (-1 if none): the bd cutover digests that
// comment's RAW text and ages the record by its bd created_at (bdsync.go).
func resolveCommentsAt(texts []string, watch string) (cur Record, at int, found, watched bool, err error) {
	at = -1
	var lastErr error
	for i, t := range texts {
		body := strings.TrimSpace(t)
		if !strings.HasPrefix(body, gonkStateMarkerPrefix) || !strings.HasSuffix(body, gonkStateMarkerSuffix) {
			continue
		}
		raw := body[len(gonkStateMarkerPrefix) : len(body)-len(gonkStateMarkerSuffix)]
		var c stateComment
		if derr := json.Unmarshal([]byte(raw), &c); derr != nil {
			lastErr = fmt.Errorf("beadstore: decode gonk-state comment: %w: %w", errUndecodableRecord, derr)
			continue
		}
		lastErr = nil
		if c.IfRevision != nil && *c.IfRevision != cur.Revision {
			continue // a conditional write that lost its race
		}
		cur, at, found = c.Record, i, true
		if watch != "" && c.Revision == watch {
			watched = true
		}
	}
	if lastErr != nil {
		return Record{}, -1, false, false, lastErr
	}
	return cur, at, found, watched, nil
}

// ErrBdDatabaseMissing means bd reached its Dolt server and the server has no
// bd database at all -- a fresh install. On a fresh Dolt, `gc init --no-start`
// writes .beads/metadata.json but leaves creating the database to `gc start`
// (gascity beads_provider_lifecycle.go, initDirIfReady), and the controller's
// import runs between the two. There are no gonk records to import; the review
// found the import failing here instead, crash-looping a fresh install forever
// (review defect A3).
//
// It is recognised from bd 1.1.0's own message, observed against the real
// binary: `Error: failed to open database: database "bd_gonk" not found on Dolt
// server at <host>:<port>`. A server that cannot be REACHED says something else
// and stays an error.
var ErrBdDatabaseMissing = errors.New("bd's Dolt database does not exist yet")

var bdDatabaseMissingRe = regexp.MustCompile(`failed to open database: database "([^"]+)" not found on Dolt server`)

// bdMissingDatabaseName is the database bd looked for and did not find, for the
// import's report: the operator can then see WHICH database was expected, since
// a misconfigured name produces this same error on an existing install.
func bdMissingDatabaseName(err error) string {
	if m := bdDatabaseMissingRe.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return ""
}

// errUndecodableRecord marks a bead whose current gonk-state comment does not
// decode: a fact about that one record, not about bd being reachable. The bd
// import (bdsync.go) skips such a record, loudly, rather than failing whole.
var errUndecodableRecord = errors.New("undecodable gonk-state record")

func stateLabel(st State) string { return gonkLabelPrefix + string(st) }

// anchorBeads enumerates EVERY bead that carries a gonk record, for the
// one-time import into the Postgres work store (gonk-t41): anchor -> bead ids,
// in bd's order, and bead id -> its labels.
//
// It asks for them by the anchor label, not by the gonk::<state> labels, so a
// bead whose state label a torn write removed (gonk-9sf9) or never wrote
// (gonk-p7qh's go-ffoe) is found too. `--limit 0` lifts bd's default of 50
// (gonk-kx3).
//
// It keeps bd's DEFAULT STATUS FILTER (no `--all`), deliberately: that is the
// filter findBeadID's `bd list --label` applies, so the import sees exactly the
// beads the bd-backed gonk-gate's Get and List could see. A closed bead's
// record was invisible to that binary; importing it would resurrect state
// nothing was acting on. Flags checked against the real bd 1.1.0
// (docs/plans/2026-09-14-t41-postgres-workstore.md section 9).
func (b *BdCLI) anchorBeads(ctx context.Context) (anchors map[string][]string, labels map[string][]string, err error) {
	out, err := b.run(ctx, "list", "--label-pattern", anchorLabelPrefix+"*", "--limit", "0", "--json")
	if err != nil {
		if bdDatabaseMissingRe.MatchString(err.Error()) {
			return nil, nil, fmt.Errorf("%w: %v", ErrBdDatabaseMissing, err)
		}
		return nil, nil, err
	}
	var rows []struct {
		ID     string   `json:"id"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil {
		return nil, nil, fmt.Errorf("beadstore: decode bd list: %w", err)
	}
	anchors, labels = map[string][]string{}, map[string][]string{}
	for _, row := range rows {
		labels[row.ID] = row.Labels
		for _, l := range row.Labels {
			if a, ok := strings.CutPrefix(l, anchorLabelPrefix); ok && a != "" {
				anchors[a] = append(anchors[a], row.ID)
			}
		}
	}
	return anchors, labels, nil
}

// labelList returns id's labels.
func (b *BdCLI) labelList(ctx context.Context, id string) ([]string, error) {
	out, err := b.run(ctx, "label", "list", id, "--json")
	if err != nil {
		return nil, fmt.Errorf("beadstore: bd label list: %w", err)
	}
	var labels []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &labels); err != nil {
		return nil, fmt.Errorf("beadstore: decode bd label list: %w", err)
	}
	return labels, nil
}

func (b *BdCLI) addLabel(ctx context.Context, id, label string) error {
	_, err := b.run(ctx, "label", "add", id, label)
	return err
}

// removeGonkStateLabels drops each `gonk::` label in labels except keep, by its
// literal name.
//
// *** REAL FINDING, confirmed against the real binary: `bd label remove
// <id> "gonk::*"` does NOT glob-expand. *** It removes (or, if no label
// literally equals the six-character string "gonk::*", no-ops on) exactly
// the literal label "gonk::*" -- the real `gonk::<state>` label is left in
// place. `bd label` has no server-side glob/prefix removal at all, so the
// caller enumerates the bead's real labels via `bd label list --json` and this
// removes each `gonk::`-prefixed one individually.
func (b *BdCLI) removeGonkStateLabels(ctx context.Context, id string, labels []string, keep string) error {
	for _, l := range labels {
		if !strings.HasPrefix(l, gonkLabelPrefix) || l == keep {
			continue
		}
		if _, err := b.run(ctx, "label", "remove", id, l); err != nil {
			return fmt.Errorf("beadstore: bd label remove %s: %w", l, err)
		}
	}
	return nil
}

func (b *BdCLI) create(ctx context.Context, anchor string) (string, error) {
	created, err := b.run(ctx, "create", "--title", anchor, "--labels", anchorLabel(anchor), "--json")
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(created), &out); err != nil {
		return "", fmt.Errorf("beadstore: decode bd create: %w", err)
	}
	return out.ID, nil
}

func (b *BdCLI) appendState(ctx context.Context, id string, c stateComment) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("beadstore: encode record: %w", err)
	}
	_, err = b.run(ctx, "comment", id, gonkStateMarkerPrefix+string(payload)+gonkStateMarkerSuffix)
	return err
}

// settleLabels is EVERY write's last step: re-read the current record and make
// sure the bead carries its label, adding it if a removal took it away. It only
// ever adds.
//
// It exists because removals race. A writer removes the labels it saw at its
// start as stale, but one of them can belong to a writer still in flight: the
// independent review of this change found the incident's own interleaving (a
// dispatch's Put adds `pending-prompt` and is preempted, a sweep's PutIf runs to
// completion and removes that label, the dispatch appends its reservation and
// removes `running`) leaving the current record with no gonk:: label at all --
// found by no List, ever.
//
// Why ending every write this way is enough when no writer is killed: take the
// last label removal any writer makes. That writer settles after it, so if no
// comment is appended after its re-read, it restores the final record's label.
// If one is, that comment's writer appended after the removal, and settles after
// its own append -- which is when the final record is visible. What is left is a
// writer that does not reach its settle after a removal or an append: killed, or
// returning early because a bd call FAILED (a removal, the post-append re-read,
// a comment that committed but reported an error -- not rare under Dolt
// slowness). List's repair cannot reach a bead that leaves unlabelled, and that
// is stated rather than hidden.
func (b *BdCLI) settleLabels(ctx context.Context, id string) error {
	texts, err := b.comments(ctx, id)
	if err != nil {
		return err
	}
	cur, found, _, err := resolveComments(texts, "")
	if err != nil || !found {
		return err
	}
	labels, err := b.labelList(ctx, id)
	if err != nil {
		return err
	}
	if want := stateLabel(cur.State); !hasString(labels, want) {
		return b.addLabel(ctx, id, want)
	}
	return nil
}

// Put upserts on r.BeadAnchor: find-or-create the bd bead, append its
// gonk-state comment, and move its gonk::<state> label.
//
// It stamps UpdatedAt and Revision first (stampWrite -- the same rule
// Memory.Put applies). It did not use to stamp UpdatedAt: it marshalled the
// caller's record verbatim, which meant every call site that forgot to set it
// wrote a zero time to the REAL store while the Memory-backed tests, which did
// stamp, stayed green. gonk-sweep's pending-prompt reclaim ages records by that
// field.
//
// THE ORDER OF THE bd CALLS IS THE RECOVERY STORY, because they are separate
// processes and any of them can be the last one that runs (a killed order, a
// Dolt outage -- gonk-p7qh stranded go-ffoe exactly this way):
//
//  1. `label list`  -- what to remove at the end. Nothing written yet.
//  2. `label add`   -- the NEW state's label, before the record exists.
//  3. `comment`     -- the record. From here it is current.
//  4. `label remove` of the labels step 1 saw, except the new one.
//  5. settleLabels: re-read the current record, add its label if missing.
//
// At every kill point the bead carries the label of whichever record is
// current: before 3 that is the old record and its old label is untouched;
// after 3 the new label is already there. The old order (comment, remove, add)
// left a bead between 3 and the final add carrying a label its record
// contradicted -- or no gonk:: label at all -- and List filters on both, so no
// pass ever looked at it again. An extra stale label left by a kill between 3
// and 4 is harmless: List skips (and repairs) a bead whose record disagrees.
//
// Removing only what step 1 saw, rather than re-listing after the comment,
// means a label a concurrent writer adds AFTER step 1 is left alone. One it
// added before step 1 can still be removed while that writer is in flight,
// which is why step 5 exists (see settleLabels).
func (b *BdCLI) Put(ctx context.Context, r Record) error {
	r = stampWrite(r, time.Now().UTC())
	id, ok, err := b.findBeadID(ctx, r.BeadAnchor)
	if err != nil {
		return err
	}
	if !ok {
		if id, err = b.create(ctx, r.BeadAnchor); err != nil {
			return err
		}
	}
	before, err := b.labelList(ctx, id)
	if err != nil {
		return err
	}
	want := stateLabel(r.State)
	if !hasString(before, want) {
		if err := b.addLabel(ctx, id, want); err != nil {
			return err
		}
	}
	if err := b.appendState(ctx, id, stateComment{Record: r}); err != nil {
		return err
	}
	if err := b.removeGonkStateLabels(ctx, id, before, want); err != nil {
		return err
	}
	return b.settleLabels(ctx, id)
}

// PutIf is Put with a precondition, and for BdCLI the precondition CLOSES the
// race for the record's content rather than merely narrowing it.
//
// A check-then-write alone could not: between this process reading the
// current revision and its comment landing there are several bd processes and,
// under the Dolt slowness gonk-p8w8 happened in, tens of seconds. So the
// comment is appended CONDITIONAL (IfRevision), and resolveComments -- the
// read rule every Get and List uses -- ignores it if anything else became
// current first. A lost race therefore cannot overwrite anything, even when
// this process was overtaken after its check.
//
// The steps:
//
//  1. read the current record; refuse early (ErrStale) if the revision already
//     moved. An optimisation, not the guarantee -- it keeps doomed comments
//     out of the log in the common case.
//  2. label list, add the new state's label (Put's ordering, same reason).
//  3. append the conditional comment.
//  4. read the comments again: did our revision take effect? If not, undo the
//     label we added and return ErrStale.
//  5. remove the labels step 2 saw (and ours, if a later write superseded
//     it) other than the current record's, then settleLabels.
//
// WHAT IS STILL NOT ATOMIC, stated plainly: the LABELS. They are a search index
// maintained by separate calls, so a kill or an interleaving can leave one
// disagreeing with the record. With every write ending in settleLabels, two
// unkilled writers leave the current record's label present; a disagreeing
// extra label is recoverable (List skips and repairs it). A writer killed, or
// failing a bd call, between a label removal or append and its settle can still
// leave the current record unlabelled, and no List can see that bead.
//
// AND THE ONE CONTENT HOLE: COMMIT ORDER. bd orders comments by a timestamp its
// client takes inside the insert's transaction, not by commit. If another
// write's insert took an EARLIER timestamp but commits AFTER step 4 read the
// log, it sorts before this write when it lands, and the fold is recomputed:
//   - it was unconditional: it applies first, this conditional write's
//     IfRevision no longer matches, and this write -- reported applied --
//     retroactively loses. Its caller did what a winner does.
//   - it was ALSO conditional on the same revision (since gonk-y4m7 the
//     dispatch's reservation is a PutIf too): same result, and now BOTH writers
//     can have been told "applied". A sweep can fire while a dispatch holds a
//     reservation that no reader will ever see.
//
// It needs the two inserts to overlap with the late one's commit stalled past
// a whole bd process round trip -- the kind of stall the Dolt pressure behind
// gonk-p8w8 produced. Closing it needs a commit-ordered sequence from the
// store, which bd does not expose (gonk-vred).
func (b *BdCLI) PutIf(ctx context.Context, r Record, ifRevision string) (string, error) {
	r = stampWrite(r, time.Now().UTC())
	id, ok, err := b.findBeadID(ctx, r.BeadAnchor)
	if err != nil {
		return "", err
	}
	if !ok {
		if ifRevision != "" {
			return "", fmt.Errorf("%w: %s does not exist, the write expected revision %q",
				ErrStale, r.BeadAnchor, ifRevision)
		}
		if id, err = b.create(ctx, r.BeadAnchor); err != nil {
			return "", err
		}
	} else {
		texts, err := b.comments(ctx, id)
		if err != nil {
			return "", err
		}
		cur, _, _, err := resolveComments(texts, "")
		if err != nil {
			return "", err
		}
		if cur.Revision != ifRevision {
			return "", fmt.Errorf("%w: %s is at revision %q, the write expected %q",
				ErrStale, r.BeadAnchor, cur.Revision, ifRevision)
		}
	}

	before, err := b.labelList(ctx, id)
	if err != nil {
		return "", err
	}
	want := stateLabel(r.State)
	added := false
	if !hasString(before, want) {
		if err := b.addLabel(ctx, id, want); err != nil {
			return "", err
		}
		added = true
	}
	cond := ifRevision
	if err := b.appendState(ctx, id, stateComment{Record: r, IfRevision: &cond}); err != nil {
		return "", err
	}

	texts, err := b.comments(ctx, id)
	if err != nil {
		return "", err
	}
	if !containsRevision(texts, r.Revision) {
		// `bd comment` succeeded but the comment is not in the log. That is not
		// a lost race and must not be reported as one: the caller would take
		// "someone else moved on" as settled, when nothing is known.
		return "", fmt.Errorf("beadstore: %s: the gonk-state comment for revision %q was written but is not in bd's comment log",
			r.BeadAnchor, r.Revision)
	}
	cur, found, applied, err := resolveComments(texts, r.Revision)
	if err != nil {
		return "", err
	}
	curLabel := stateLabel(cur.State)
	if !applied {
		// Lost after the check. Our comment is inert; take back the label we
		// added unless the record that won happens to want it too.
		if added && (!found || curLabel != want) {
			if err := b.removeGonkStateLabels(ctx, id, []string{want}, ""); err != nil {
				return "", err
			}
		}
		if err := b.settleLabels(ctx, id); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: %s moved to revision %q while this write was in flight (expected %q)",
			ErrStale, r.BeadAnchor, cur.Revision, ifRevision)
	}
	// Applied. If a later unconditional write has already superseded it, its
	// label is the one to keep and ours goes with the rest; settleLabels then
	// guarantees the current record's label whichever way that went.
	have := before
	if added {
		have = append(append([]string(nil), before...), want)
	}
	if err := b.removeGonkStateLabels(ctx, id, have, curLabel); err != nil {
		return "", err
	}
	if err := b.settleLabels(ctx, id); err != nil {
		return "", err
	}
	return r.Revision, nil
}

func (b *BdCLI) Get(ctx context.Context, anchor string) (Record, bool, error) {
	id, ok, err := b.findBeadID(ctx, anchor)
	if err != nil || !ok {
		return Record{}, ok, err
	}
	return b.getByID(ctx, id)
}

// comments returns the text of every comment on id, in bd's order.
func (b *BdCLI) comments(ctx context.Context, id string) ([]string, error) {
	rows, err := b.commentRows(ctx, id)
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.Text
	}
	return texts, nil
}

// bdComment is one row of `bd comments <id> --json`. CreatedAt is bd's own
// created_at (bd 1.1.0: RFC 3339, whole seconds), zero if bd did not send one.
type bdComment struct {
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

func (b *BdCLI) commentRows(ctx context.Context, id string) ([]bdComment, error) {
	out, err := b.run(ctx, "comments", id, "--json")
	if err != nil {
		return nil, err
	}
	// REAL FINDING, confirmed against the real binary: `bd comments <id>
	// --json` puts the comment text under the field name "text", NOT "body".
	// The earlier guess (`json:"body"`) silently decoded to an empty string
	// on every row, so Get/List/getByID NEVER found an existing gonk-state
	// comment -- BdCLI.Get would unconditionally report "not found" against
	// a real bd store, no matter how many times Put had run.
	var rows []bdComment
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil {
		return nil, fmt.Errorf("beadstore: decode bd comments: %w", err)
	}
	return rows, nil
}

// bdRecord is one bead's current gonk record as the bd cutover needs it: the
// decoded Record, the RAW text of the comment it came from, and when bd says
// that comment was written.
type bdRecord struct {
	ID        string // the bead the record was read from
	Record    Record
	Raw       string
	WrittenAt time.Time
	Found     bool
}

// readRecord resolves bead id's current record exactly as getByID does.
func (b *BdCLI) readRecord(ctx context.Context, id string) (bdRecord, error) {
	rows, err := b.commentRows(ctx, id)
	if err != nil {
		return bdRecord{}, err
	}
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.Text
	}
	cur, at, found, _, err := resolveCommentsAt(texts, "")
	if err != nil || !found {
		return bdRecord{}, err
	}
	return bdRecord{ID: id, Record: cur, Raw: strings.TrimSpace(rows[at].Text), WrittenAt: rows[at].CreatedAt, Found: true}, nil
}

// bdListDefaultLimit is bd 1.1.0's `bd list` default (`-n, --limit int ...
// (default 50, use 0 for unlimited)`), which BdCLI.List never overrode (gonk-kx3).
const bdListDefaultLimit = 50

// listedBy returns the beads the bd-backed gonk-gate's List(st) could see: the
// same `bd list --label gonk::<st>` call, with bd's default limit written out.
// For the cutover's quarantine (bdsync.go): a record this call does not return
// is one no bd-backed sweep ever judged.
func (b *BdCLI) listedBy(ctx context.Context, st State) (map[string]bool, error) {
	out, err := b.run(ctx, "list", "--label", stateLabel(st), "--limit", strconv.Itoa(bdListDefaultLimit), "--json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil {
		return nil, fmt.Errorf("beadstore: decode bd list: %w", err)
	}
	ids := make(map[string]bool, len(rows))
	for _, r := range rows {
		ids[r.ID] = true
	}
	return ids, nil
}

func (b *BdCLI) getByID(ctx context.Context, id string) (Record, bool, error) {
	texts, err := b.comments(ctx, id)
	if err != nil {
		return Record{}, false, err
	}
	r, ok, _, err := resolveComments(texts, "")
	return r, ok, err
}

// List returns the records whose CURRENT state is st.
//
// It finds candidates by the gonk::<st> label and then believes the record,
// not the label: a bead whose label says st while its record says otherwise is
// not returned. That disagreement is what a torn or overtaken write leaves
// behind, and it used to be terminal -- the bead matched no List at all
// (gonk-p7qh's go-ffoe). So List also REPAIRS it, by adding the label of the
// record's actual state, which the pass that lists that state then finds.
//
// Repair only ever ADDS. Removing the stale label here could race a writer
// that has added its new label but not yet appended its record: this pass
// would see the old record, strip the label the writer is about to need, and
// the writer's own cleanup would then strip the old one -- leaving no gonk::
// label at all. An extra label costs one comment read per pass until the next
// write on that bead removes it.
//
// Best effort: a failed repair does not fail the List (the next pass retries).
func (b *BdCLI) List(ctx context.Context, st State) ([]Record, error) {
	out, err := b.run(ctx, "list", "--label", stateLabel(st), "--json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID     string   `json:"id"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil {
		return nil, fmt.Errorf("beadstore: decode bd list: %w", err)
	}
	recs := make([]Record, 0, len(rows))
	for _, row := range rows {
		r, ok, err := b.getByID(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if r.State == st {
			recs = append(recs, r)
			continue
		}
		if l := stateLabel(r.State); !hasString(row.Labels, l) {
			_ = b.addLabel(ctx, row.ID, l)
		}
	}
	return recs, nil
}

// ListOpenPipelineRuns is Store.ListOpenPipelineRuns. bd cannot query a
// record field, so this lists every state and filters: the full read the
// method exists to avoid. That is acceptable only because gonk-gate no longer
// selects BdCLI for its state (gonk-t41); it is here so BdCLI stays a Store.
func (b *BdCLI) ListOpenPipelineRuns(ctx context.Context) ([]Record, error) {
	var out []Record
	for _, st := range AllStates {
		recs, err := b.List(ctx, st)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if r.PipelineRunOpen {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BeadAnchor < out[j].BeadAnchor })
	return out, nil
}

func containsRevision(texts []string, rev string) bool {
	for _, t := range texts {
		if strings.Contains(t, rev) {
			return true
		}
	}
	return false
}

func hasString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
