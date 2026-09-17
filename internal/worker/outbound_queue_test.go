package worker

import "testing"

func TestOutboundQueueCloseDrainsBufferedMessagesAndStopsSends(t *testing.T) {
	queue := newOutboundQueue(1)
	message := []byte("message")
	if err := queue.Send(message); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	queue.Close()
	if got := <-queue.Messages(); string(got) != string(message) {
		t.Fatalf("message = %q, want %q", got, message)
	}
	if _, ok := <-queue.Messages(); ok {
		t.Fatal("queue remained open after buffered messages drained")
	}
	if err := queue.Send([]byte("late")); err == nil {
		t.Fatal("Send() after Close() returned nil")
	}
}
