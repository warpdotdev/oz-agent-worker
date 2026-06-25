// Package commandbackendexample holds reference dispatch commands for the
// oz-agent-worker "command" backend. dispatch.py / cancel.py transform the task
// payload and delegate execution to an HTTP REST endpoint; dispatch-oz-local.py
// launches a real oz agent run on the host for local end-to-end testing. The
// Python scripts are the actual artifacts; the Go test in this package verifies
// their transformation, launch behavior, and exit-code contracts.
package commandbackendexample
