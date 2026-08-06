// Package debuglog implements the worker half of the REMOTE-2516 debug-archive
// log protocol: request validation, bounded NDJSON snapshot encoding, secure
// disk-backed capture for the direct backend, upload to a server-supplied
// target, and the asynchronous coordinator that ties them together.
//
// Nothing in this package may log captured bytes, upload targets, signed
// headers or form fields, local capture paths, or upload response bodies.
package debuglog

// SchemaVersion is the NDJSON schema every snapshot record carries.
const SchemaVersion = 1

// Record kinds emitted into a snapshot.
const (
	KindData        = "data"
	KindSourceError = "source_error"
	KindTruncation  = "truncation"
)

// Data encodings. A chunk that is valid UTF-8 is stored directly; anything
// else is base64 so the NDJSON stream stays valid.
const (
	EncodingUTF8   = "utf8"
	EncodingBase64 = "base64"
)

// Stream identifies which of a source's streams a chunk came from. A provider
// that merges its streams reports StreamCombined, and one that cannot say at
// all reports StreamUnknown; neither is ever inferred from log text.
type Stream string

const (
	StreamStdout   Stream = "stdout"
	StreamStderr   Stream = "stderr"
	StreamCombined Stream = "combined"
	StreamUnknown  Stream = "unknown"
)

// Phase is provider truth about which part of an execution produced a chunk,
// not a process classifier. Only the direct backend owns distinct
// setup/agent/teardown handles; container providers report PhaseContainer.
type Phase string

const (
	PhaseSetup     Phase = "setup"
	PhaseAgent     Phase = "agent"
	PhaseTeardown  Phase = "teardown"
	PhaseContainer Phase = "container"
)

// Backend kinds reported on every record and in the acknowledgement.
const (
	BackendDocker     = "docker"
	BackendKubernetes = "kubernetes"
	BackendDirect     = "direct"
	BackendCommand    = "command"
)

// MaxChunkBytes bounds how much decoded data one NDJSON record may carry, so
// a single provider read can never produce an unbounded line.
const MaxChunkBytes = 32 * 1024

// TruncationPolicyFirstLast names the only bytes-dropped policy V1 implements.
const TruncationPolicyFirstLast = "first_last"

// SourceIdentity carries the provider identity a backend actually supplies for
// a chunk. Fields the provider does not supply stay empty rather than being
// guessed.
type SourceIdentity struct {
	// ContainerID is set by the Docker backend.
	ContainerID string
	// Namespace, Pod, Container, ContainerType, RestartAttempt, and Previous
	// are set by the Kubernetes backend.
	Namespace      string
	Pod            string
	Container      string
	ContainerType  string
	RestartAttempt *int32
	Previous       bool
}

// dataRecord is the wire shape of a schema-v1 data record. Only Data passes
// through the request's content transformer; every other field is structural.
type dataRecord struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	Sequence       int64  `json:"sequence"`
	Backend        string `json:"backend"`
	Phase          string `json:"phase"`
	Stream         string `json:"stream"`
	Timestamp      string `json:"timestamp,omitempty"`
	ObservedAt     string `json:"observed_at"`
	Encoding       string `json:"encoding"`
	Data           string `json:"data"`
	ContainerID    string `json:"container_id,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	Pod            string `json:"pod,omitempty"`
	Container      string `json:"container,omitempty"`
	ContainerType  string `json:"container_type,omitempty"`
	RestartAttempt *int32 `json:"restart_attempt,omitempty"`
	Previous       bool   `json:"previous,omitempty"`
}

// sourceErrorRecord reports that one source could not be read. It carries only
// safe source identity plus a stable warning code, never the provider's error.
type sourceErrorRecord struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	Sequence       int64  `json:"sequence"`
	Backend        string `json:"backend"`
	Phase          string `json:"phase"`
	Stream         string `json:"stream"`
	ObservedAt     string `json:"observed_at"`
	WarningCode    string `json:"warning_code"`
	ContainerID    string `json:"container_id,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	Pod            string `json:"pod,omitempty"`
	Container      string `json:"container,omitempty"`
	ContainerType  string `json:"container_type,omitempty"`
	RestartAttempt *int32 `json:"restart_attempt,omitempty"`
	Previous       bool   `json:"previous,omitempty"`
}

// truncationRecord marks the gap between the retained first and last portions
// of a bounded snapshot.
type truncationRecord struct {
	SchemaVersion       int    `json:"schema_version"`
	Kind                string `json:"kind"`
	Policy              string `json:"policy"`
	OmittedBytesAtLeast int64  `json:"omitted_bytes_at_least"`
}
