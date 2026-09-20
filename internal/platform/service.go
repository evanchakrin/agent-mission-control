package platform

import (
	"context"
	"fmt"
)

func invoke(ctx context.Context, run func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("service panic: %v", p)
		}
	}()
	return run(ctx)
}
