package performance

import (
	"errors"
	"os"
	"runtime"
)

// CreateWitnessTarget exclusively creates <run_id>.witness-data.bin inside an
// already selected private directory. The caller owns both returned handles and
// supplies them together to NewWitnessFileWriter, then closes them after use.
// Creation does not sync or admit writes; identity, ownership and fault scope
// must be established independently. Failure retains any newly created file.
func CreateWitnessTarget(root *os.Root, plan WitnessFilePlan) (target, directory *os.File, err error) {
	directory, err = openWitnessDirectory(root, plan)
	if err != nil {
		return nil, nil, err
	}
	// O_EXCL and a single component refuse existing files and final symlinks.
	// Both opens use the same pinned root, including after a directory rename.
	target, err = root.OpenFile(witnessTargetName(plan), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		reason := ErrEvidenceIncomplete
		if errors.Is(err, os.ErrExist) {
			reason = ErrEvidenceExists
		}
		return nil, nil, errors.Join(reason, closeWitnessFiles(directory))
	}
	info, statErr := target.Stat()
	if statErr != nil || info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != 0 {
		return nil, nil, errors.Join(ErrEvidenceIncomplete, closeWitnessFiles(target, directory))
	}
	return target, directory, nil
}

// OpenWitnessSource opens the same run filename read-only without following a
// final symlink or blocking on a FIFO. It returns a private regular file within
// the plan's capacity, without reading payloads or establishing source identity.
// The caller owns the handle and must ensure identity/exclusion before recovery.
func OpenWitnessSource(root *os.Root, plan WitnessFilePlan) (*os.File, error) {
	directory, err := openWitnessDirectory(root, plan)
	if err != nil {
		return nil, err
	}
	file, openErr := openEvidenceFile(directory, witnessTargetName(plan))
	closeErr := closeWitnessFiles(directory)
	if openErr != nil || closeErr != nil {
		return nil, errors.Join(openErr, closeErr, closeWitnessFiles(file))
	}
	info, statErr := file.Stat()
	if statErr != nil || info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.Join(ErrEvidenceCorrupt, closeWitnessFiles(file))
	}
	size := info.Size()
	if size < 0 || uint64(size) > plan.CapacityBytes {
		return nil, errors.Join(ErrEvidenceCorrupt, closeWitnessFiles(file))
	}
	return file, nil
}

func openWitnessDirectory(root *os.Root, plan WitnessFilePlan) (*os.File, error) {
	if !validWitnessFilePlan(plan) {
		return nil, ErrInvalidWitness
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil, ErrEvidencePlatform
	}
	if root == nil {
		return nil, ErrEvidenceRoot
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, ErrEvidenceRoot
	}
	info, statErr := directory.Stat()
	if statErr != nil || info == nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.Join(ErrEvidenceRoot, closeWitnessFiles(directory))
	}
	return directory, nil
}

func witnessTargetName(plan WitnessFilePlan) string {
	return plan.Stream.RunID + ".witness-data.bin"
}

func closeWitnessFiles(files ...*os.File) error {
	failed := false
	for _, file := range files {
		if file != nil && file.Close() != nil {
			failed = true
		}
	}
	if failed {
		return ErrEvidenceIncomplete
	}
	return nil
}
