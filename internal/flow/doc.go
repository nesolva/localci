// Package flow is the core's half of reusable flows (docs/protocol.md, "Flows and checkpoints"): the test package
// calls POST $LOCALCI_API/hooks/flow/{lookup|save|restore} around every flow with a checkpoint, and this package
// decides whether a saved result can be restored, keeps the results (browser storage state, outputs and a
// database snapshot) per suite under a size cap, and summarises how flows are used for `localci flows`.
package flow
