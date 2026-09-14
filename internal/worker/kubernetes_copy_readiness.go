package worker

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	sidecarCopyStateMountPath = "/oz-copy-state"
	sidecarCopyCompletePath   = sidecarCopyStateMountPath + "/complete"
	sidecarCopyFailureMessage = "oz-sidecar-copy-failed"
)

// Completion belongs to the Pod, not a particular helper process. A restarted
// helper must not overwrite files that a running task may already be using.
// Keep this state separate from the payload, including writable sidecar mounts.
func sidecarCopyStateVolume(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func configureSidecarCopyReadiness(container *corev1.Container, stateVolume string) {
	restart := corev1.ContainerRestartPolicyAlways
	container.RestartPolicy = &restart
	container.StartupProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "test -f " + sidecarCopyCompletePath}}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 600,
	}
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: stateVolume, MountPath: sidecarCopyStateMountPath})
}

func kubernetesSidecarHoldScript() string {
	return `child=""
trap '[ -z "$child" ] || kill "$child" 2>/dev/null || true; exit 0' TERM INT
while :; do
  sleep 3600 &
  child=$!
  wait "$child"
done`
}

// A FIFO lets POSIX sh check both tar exit statuses without relying on pipefail.
// The completion marker is written only after both commands have succeeded.
func kubernetesSidecarReadinessScript() string {
	// Retain the legacy copy's exclusions and ownership behavior. The private
	// state volume must not be included in the archive it helps produce.
	producer := kubernetesSidecarArchiveCommand("oz-copy-state")
	consumer := kubernetesSidecarExtractCommand
	return `if [ ! -f ` + sidecarCopyCompletePath + ` ]; then
  producer=""
  consumer=""
  cleanup() {
    status=$?
    [ -z "$producer" ] || kill "$producer" 2>/dev/null || true
    [ -z "$consumer" ] || kill "$consumer" 2>/dev/null || true
    rm -f ` + sidecarCopyStateMountPath + `/archive.pipe
    if [ "$status" -ne 0 ] && [ ! -f ` + sidecarCopyCompletePath + ` ]; then
      touch ` + sidecarCopyStateMountPath + `/failed
      printf '%s\n' '` + sidecarCopyFailureMessage + `' > /dev/termination-log
    fi
  }
  trap cleanup EXIT
  trap 'exit 0' TERM INT
  [ ! -f ` + sidecarCopyStateMountPath + `/failed ]
  # A previous attempt may have exited halfway through extraction.
  find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
  rm -f ` + sidecarCopyStateMountPath + `/archive.pipe
  mkfifo ` + sidecarCopyStateMountPath + `/archive.pipe
  ` + producer + ` > ` + sidecarCopyStateMountPath + `/archive.pipe &
  producer=$!
  ` + consumer + ` < ` + sidecarCopyStateMountPath + `/archive.pipe &
  consumer=$!
  wait "$consumer"
  consumer=""
  wait "$producer"
  producer=""
  rm -f ` + sidecarCopyStateMountPath + `/archive.pipe
  touch ` + sidecarCopyCompletePath + `
fi
` + kubernetesSidecarHoldScript()
}
