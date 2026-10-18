//go:build !linux

package volumeprobe

import (
	"context"
	"os"
)

func observe(context.Context, Request, []*os.File) Result {
	return Result{Code: Unsupported}
}
