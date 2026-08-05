// Package portalbin names the binary that owns the Stellar and Portal verbs.
//
// Stellar and Portal moved out of the tau CLI into taugrid-portal, so every
// command string this product hands a user to run has to name taugrid-portal.
// A generated "tau experiment ..." now fails with "unknown command" on any
// post-split tau, and it fails at paste time rather than at build time, which
// is why these strings need a single owner instead of ~20 concatenations
// spread across the snapshot builder, the export packet, and CLI help.
package portalbin

// Name is the executable as installed on PATH and in the published image at
// InstallPath.
const Name = "taugrid-portal"

// InstallPath is where the taugrid-portal image installs Name. The metrics
// offload sidecar rendered by tau core execs this path, so it is a contract
// that crosses the module boundary.
const InstallPath = "/usr/local/bin/" + Name

// ExperimentCmd prefixes a runnable experiment command, e.g.
// "taugrid-portal experiment --store /data/exp status run-1".
const ExperimentCmd = Name + " experiment"
