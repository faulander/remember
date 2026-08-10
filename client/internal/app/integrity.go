package app

import (
	"context"

	"github.com/faulander/remember/client/internal/clientsync"
)

// IntegrityIncidents returns durable, unacknowledged sync integrity alarms.
func (c *LocalCore) IntegrityIncidents(ctx context.Context, limit int) ([]clientsync.IntegrityIncident, error) {
	c.lifecycleMu.Lock()
	closed := c.closed
	c.lifecycleMu.Unlock()
	if closed {
		return nil, ErrCoreClosed
	}
	store, err := clientsync.NewStore(c.index)
	if err != nil {
		return nil, err
	}
	return store.ListOpenIntegrityIncidents(ctx, limit)
}
