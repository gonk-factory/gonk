package main

// fxGLPAT is a secret-SHAPED fixture: a fake GitLab token (an alphabet run,
// never valid anywhere) that the note and prompt tests plant to prove it is
// refused or redacted. It is assembled from pieces so no committed line holds
// it whole -- the public mirror's gitleaks gate fails on any finding and
// cannot tell a planted test value from a leak. The compiler joins the pieces;
// the tests see the same bytes as when it was a literal. See
// docs/releases/PRE-PUBLISH-CHECKLIST.md.
const fxGLPAT = "glp" + "at-" + "AbCdEfGhIjKlMnOpQrSt"

// fxGLPATLong is fxGLPAT two characters longer: the value the pipeline-fix
// gate test plants in an edit to prove SC-SECRET refuses it.
const fxGLPATLong = fxGLPAT + "Uv"

// fxBotToken is the fake bot token the pipeline-fix apply tests hand the gate
// as an EXACT secret (effects.NewSecret) and then plant in an edit. It has the
// GitLab token prefix on purpose and is assembled for the same reason as
// fxGLPAT.
const fxBotToken = "glp" + "at-" + "not-a-real-token-0000"
