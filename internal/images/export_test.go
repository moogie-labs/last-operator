package images

import "time"

// Tune shortens the poll interval and the wait slack for tests.
func (b *Builder) Tune(poll, waitSlack time.Duration) {
	b.poll, b.waitSlack = poll, waitSlack
}
