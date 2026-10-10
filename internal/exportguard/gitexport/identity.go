package gitexport

// The one identity that may appear on a public snapshot commit or tag: the
// identity already on the mirror's existing commits. It is a constant, not a
// flag and not git configuration, because the commit id is a function of it.
const (
	IdentityName  = "Steven Wagner"
	IdentityEmail = "leftathome@gmail.com"
)

// There is deliberately NO baseline parent here. Until 2026-10-08 a constant
// named the head of the public mirror as the one commit a first snapshot
// could go on top of. That mirror was deleted and recreated empty; the tree
// it named exists nowhere public, so nothing may trust it as a parent. A
// first snapshot is now a declared root export to an empty mirror
// (PublishOptions.Root), and every later one is fenced by the previous
// release's source (PublishOptions.PrevSource).
