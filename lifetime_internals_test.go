package acp

import (
	"testing"
	"testing/synctest"
)

// A transport may need handler work to stop before its Close can finish. The
// connection owns both, so cancellation must flow outward before transport
// release rather than making each side wait for the other.
func TestLifetimeCancelsWorkBeforeReleasingTheTransport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		life := newLifetime()
		escape := make(chan struct{})
		cancelledBeforeRelease := make(chan bool, 1)
		ended := make(chan struct{})
		go func() {
			life.endReading(nil, func() error {
				select {
				case <-life.ctx.Done():
					cancelledBeforeRelease <- true
				case <-escape:
					cancelledBeforeRelease <- false
				}
				return nil
			})
			close(ended)
		}()

		synctest.Wait()
		select {
		case cancelled := <-cancelledBeforeRelease:
			if !cancelled {
				t.Fatal("transport release began before connection work was cancelled")
			}
		default:
			close(escape)
			<-ended
			t.Fatal("transport release was waiting on work whose context was not cancelled")
		}
		<-ended
		life.finishDelivering(func() {})
		if err := life.wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}
