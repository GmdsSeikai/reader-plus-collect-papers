package reader

import "context"

// A dispatch hook belongs to one request, never to a reused chat session.
// Before closes the reservation-to-native-delivery boundary; Sent confirms a
// complete write. A failed partial write remains uncertain for recovery.
type generationDispatch struct {
	Before       func(context.Context) error
	Sent         func() error
	NotDelivered func() error
}

func (d *generationDispatch) deliver(ctx context.Context, write func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d != nil && d.Before != nil {
		if err := d.Before(ctx); err != nil {
			return err
		}
	}
	if err := write(); err != nil {
		return err
	}
	if d != nil && d.Sent != nil {
		return d.Sent()
	}
	return nil
}
