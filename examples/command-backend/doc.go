// Package commandbackendexample holds a reference dispatch command for the
// oz-agent-worker "command" backend that transforms the task payload and
// delegates execution to an HTTP REST endpoint. The Python scripts (dispatch.py,
// cancel.py) are the actual artifacts; the Go test in this package verifies the
// reference dispatch script's transformation and exit-code contract.
package commandbackendexample
