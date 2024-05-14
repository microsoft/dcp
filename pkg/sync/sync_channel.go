package sync

import "context"

type syncChannel struct {
	ch chan struct{}
}

func NewSyncChannel() *syncChannel {
	return &syncChannel{
		ch: make(chan struct{}, 1),
	}
}

func (sc *syncChannel) Lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case sc.ch <- struct{}{}:
	}

	// guard against possible race condition where the context expires and mutex locks at the same time
	if ctx.Err() != nil {
		sc.Unlock()
		return ctx.Err()
	}

	return nil
}

func (sc *syncChannel) Unlock() {
	<-sc.ch
}
