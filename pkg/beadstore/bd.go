package beadstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// BdCLI shells the MIT `bd` binary against the shared Dolt bead store (spec
// 4.2) instead of holding state in memory. It stores a Record as a single
// structured comment on the bead -- `<!-- gonk-state {json} -->`, the last
// one wins on read -- plus a `gonk::<state>` label so a human can see a
// bead's gate state in the beads UI without decoding the comment.
//
// *** THE EXACT SUBCOMMANDS BELOW ARE CONFIRMED AGAINST THE REAL `bd` 1.0.3
// BINARY *** (Task 6 Step 5, docs/environment.md's dev-box `bd` and the
// pinned BD_VERSION in images/versions.env agree). Three invocations an
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

func anchorLabel(anchor string) string { return "gonk-anchor:" + anchor }

// removeGonkStateLabels drops every existing `gonk::<state>` label from id
// before Put adds the new one, so a bead never accumulates more than one
// state label as it moves through the gate's state machine.
//
// *** REAL FINDING, confirmed against the real binary: `bd label remove
// <id> "gonk::*"` does NOT glob-expand. *** It removes (or, if no label
// literally equals the six-character string "gonk::*", no-ops on) exactly
// the literal label "gonk::*" -- the real `gonk::<state>` label is left in
// place. `bd label` has no server-side glob/prefix removal at all, so this
// enumerates the bead's real labels via `bd label list --json` and removes
// each `gonk::`-prefixed one individually.
func (b *BdCLI) removeGonkStateLabels(ctx context.Context, id string) error {
	out, err := b.run(ctx, "label", "list", id, "--json")
	if err != nil {
		return fmt.Errorf("beadstore: bd label list: %w", err)
	}
	var labels []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &labels); err != nil {
		return fmt.Errorf("beadstore: decode bd label list: %w", err)
	}
	for _, l := range labels {
		if !strings.HasPrefix(l, gonkLabelPrefix) {
			continue
		}
		if _, err := b.run(ctx, "label", "remove", id, l); err != nil {
			return fmt.Errorf("beadstore: bd label remove %s: %w", l, err)
		}
	}
	return nil
}

// Put upserts on r.BeadAnchor: find-or-create the bd bead, then overwrite its
// gonk-state comment and its gonk::<state> label.
func (b *BdCLI) Put(ctx context.Context, r Record) error {
	id, ok, err := b.findBeadID(ctx, r.BeadAnchor)
	if err != nil {
		return err
	}
	if !ok {
		created, err := b.run(ctx, "create", "--title", r.BeadAnchor, "--labels", anchorLabel(r.BeadAnchor), "--json")
		if err != nil {
			return err
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(created), &out); err != nil {
			return fmt.Errorf("beadstore: decode bd create: %w", err)
		}
		id = out.ID
	}

	payload, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("beadstore: encode record: %w", err)
	}
	comment := gonkStateMarkerPrefix + string(payload) + gonkStateMarkerSuffix
	if _, err := b.run(ctx, "comment", id, comment); err != nil {
		return err
	}
	if err := b.removeGonkStateLabels(ctx, id); err != nil {
		return err
	}
	if _, err := b.run(ctx, "label", "add", id, gonkLabelPrefix+string(r.State)); err != nil {
		return err
	}
	return nil
}

func (b *BdCLI) Get(ctx context.Context, anchor string) (Record, bool, error) {
	id, ok, err := b.findBeadID(ctx, anchor)
	if err != nil || !ok {
		return Record{}, ok, err
	}
	return b.getByID(ctx, id)
}

func (b *BdCLI) getByID(ctx context.Context, id string) (Record, bool, error) {
	out, err := b.run(ctx, "comments", id, "--json")
	if err != nil {
		return Record{}, false, err
	}
	// REAL FINDING, confirmed against the real binary: `bd comments <id>
	// --json` puts the comment text under the field name "text", NOT "body".
	// The earlier guess (`json:"body"`) silently decoded to an empty string
	// on every row, so Get/List/getByID NEVER found an existing gonk-state
	// comment -- BdCLI.Get would unconditionally report "not found" against
	// a real bd store, no matter how many times Put had run.
	var rows []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil {
		return Record{}, false, fmt.Errorf("beadstore: decode bd comments: %w", err)
	}
	// Last matching comment wins: a bead accumulates one gonk-state comment
	// per Put, and only the newest reflects the current record.
	for i := len(rows) - 1; i >= 0; i-- {
		body := strings.TrimSpace(rows[i].Text)
		if !strings.HasPrefix(body, gonkStateMarkerPrefix) || !strings.HasSuffix(body, gonkStateMarkerSuffix) {
			continue
		}
		raw := body[len(gonkStateMarkerPrefix) : len(body)-len(gonkStateMarkerSuffix)]
		var r Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return Record{}, false, fmt.Errorf("beadstore: decode gonk-state comment: %w", err)
		}
		return r, true, nil
	}
	return Record{}, false, nil
}

func (b *BdCLI) List(ctx context.Context, st State) ([]Record, error) {
	out, err := b.run(ctx, "list", "--label", gonkLabelPrefix+string(st), "--json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID string `json:"id"`
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
		if ok && r.State == st {
			recs = append(recs, r)
		}
	}
	return recs, nil
}
