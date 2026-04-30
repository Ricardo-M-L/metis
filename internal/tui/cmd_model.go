package tui

// This file used to host stub handlers for /model, /effort, /fast, /cost,
// /usage. None of them were ever wired into BuildREPLCommands — the active
// implementations live in commands.go where they get *REPL and can mutate
// the live agent.Loop.
//
// Kept as a placeholder so the package layout stays stable; delete once any
// model-specific subcommand grows enough machinery to need its own file.
