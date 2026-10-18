package driver

import (
	"context"
	"os"
	"testing"

	"github.com/fenio/tns-csi/pkg/volumeprobe"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == volumeprobe.HelperArgument {
		os.Exit(volumeprobe.RunHelper(context.Background(), os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}
