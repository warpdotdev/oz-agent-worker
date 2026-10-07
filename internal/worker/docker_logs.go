package worker

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/moby/moby/client"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/tasklogs"
)

func (b *DockerBackend) followContainerLogs(ctx context.Context, containerID string, reporter *tasklogs.Reporter) func() {
	if reporter == nil {
		return func() {}
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(reporter.Context(), cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		stream, err := b.dockerClient.ContainerLogs(streamCtx, containerID, client.ContainerLogsOptions{
			ShowStdout: true, ShowStderr: true, Follow: true,
		})
		if err != nil {
			log.Warnf(ctx, "Could not follow Docker task logs")
			return
		}
		defer func() { _ = stream.Close() }()
		closeOnCancel := context.AfterFunc(streamCtx, func() { _ = stream.Close() })
		defer closeOnCancel()
		stdout, stderr := reporter.Writer(ctx, "container.stdout"), reporter.Writer(ctx, "container.stderr")
		defer stdout.Flush()
		defer stderr.Flush()
		if err := copyDockerLogFrames(stream, stdout, stderr); err != nil && !errors.Is(err, io.EOF) && streamCtx.Err() == nil {
			log.Warnf(ctx, "Docker task log stream interrupted")
		}
	}()
	return func() {
		defer cancel()
		defer stop()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			cancel()
		}
	}
}

func copyDockerLogFrames(reader io.Reader, stdout, stderr io.Writer) error {
	var header [8]byte
	for {
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return err
		}
		writer := io.Discard
		switch header[0] {
		case 1:
			writer = stdout
		case 2:
			writer = stderr
		}
		if _, err := io.CopyN(writer, reader, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			return err
		}
	}
}
