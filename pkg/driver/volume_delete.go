package driver

import (
	"context"
	"fmt"
	"path"
	"strings"

	"k8s.io/klog/v2"

	"github.com/fenio/tns-csi/pkg/tnsapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkUnmanagedDatasetAtVolumeID is called when DeleteVolume's lookup found no managed
// volume. For dataset-path volume IDs (e.g. "tank/csi/pvc-x") the ID names the dataset
// directly; if a dataset still exists there without tns-csi:managed_by=tns-csi, the
// volume was created by this driver but its properties were never written (or were
// changed). Reporting success would leak the dataset and its share/target forever, and
// deleting it is unsafe, so it fails with FailedPrecondition and a recovery hint.
// Legacy plain-name IDs are found by a property scan and cannot be checked this way.
func (s *ControllerService) checkUnmanagedDatasetAtVolumeID(ctx context.Context, volumeID string) error {
	if !isDatasetPathVolumeID(volumeID) {
		return nil
	}
	dataset, err := s.apiClient.GetDatasetWithProperties(ctx, volumeID)
	if err != nil {
		return status.Errorf(codes.Unavailable, "cannot verify whether dataset %s still exists: %v", volumeID, err)
	}
	if dataset == nil {
		return nil
	}
	managedBy := ""
	if p, ok := dataset.UserProperties[tnsapi.PropertyManagedBy]; ok {
		managedBy = p.Value
	}
	klog.Errorf("DeleteVolume %s: dataset exists but is not marked as managed by tns-csi (managed_by=%q); "+
		"refusing to delete it and refusing to report it as deleted. If this volume was created by tns-csi, run "+
		"'zfs set %s=%s %s' on TrueNAS and the deletion will be retried; otherwise remove the PV without deleting storage.",
		volumeID, managedBy, tnsapi.PropertyManagedBy, tnsapi.ManagedByValue, volumeID)
	return status.Errorf(codes.FailedPrecondition,
		"dataset %s exists but is not managed by tns-csi (managed_by=%q); set %s=%s on it to allow deletion",
		volumeID, managedBy, tnsapi.PropertyManagedBy, tnsapi.ManagedByValue)
}

// volumeOwnership is what DeleteVolume learns from a volume's ZFS user properties.
type volumeOwnership struct {
	props          map[string]string // all requested properties that were present
	deleteStrategy string            // tnsapi.DeleteStrategyDelete unless the volume says otherwise
	notFound       bool              // the dataset/ZVOL no longer exists
}

// readVolumeOwnership reads the properties that authorize deleting meta's dataset, plus
// any protocol-specific extras, and verifies that tns-csi owns the volume.
//
// It fails closed: if the properties cannot be read for any reason other than the
// dataset being gone, it returns Unavailable so the caller deletes nothing and the
// sidecar retries with backoff. Proceeding without them would silently ignore
// deleteStrategy=retain and the ownership checks.
func (s *ControllerService) readVolumeOwnership(ctx context.Context, meta *VolumeMetadata, extraProps ...string) (volumeOwnership, error) {
	names := append([]string{
		tnsapi.PropertyManagedBy,
		tnsapi.PropertyCSIVolumeName,
		tnsapi.PropertyDeleteStrategy,
	}, extraProps...)

	props, err := s.apiClient.GetDatasetProperties(ctx, meta.DatasetID, names)
	if err != nil {
		if tnsapi.IsNotFound(err) {
			return volumeOwnership{deleteStrategy: tnsapi.DeleteStrategyDelete, notFound: true}, nil
		}
		return volumeOwnership{}, status.Errorf(codes.Unavailable,
			"cannot read ownership properties of %s for volume %s: %v; refusing to delete until they can be verified",
			meta.DatasetID, meta.Name, err)
	}

	if managedBy, ok := props[tnsapi.PropertyManagedBy]; ok && managedBy != tnsapi.ManagedByValue {
		return volumeOwnership{}, status.Errorf(codes.FailedPrecondition,
			"dataset %s is not managed by tns-csi (managed_by=%s), refusing to delete", meta.DatasetID, managedBy)
	}

	// For dataset-path volume IDs (e.g. "tank/pvc-xxx") the stored name is just "pvc-xxx".
	if stored, ok := props[tnsapi.PropertyCSIVolumeName]; ok {
		if stored != meta.Name && (!isDatasetPathVolumeID(meta.Name) || !strings.HasSuffix(meta.Name, "/"+stored)) {
			return volumeOwnership{}, status.Errorf(codes.FailedPrecondition,
				"dataset %s belongs to volume %q, not %q (possible ID reuse), refusing to delete",
				meta.DatasetID, stored, meta.Name)
		}
	}

	o := volumeOwnership{props: props, deleteStrategy: tnsapi.DeleteStrategyDelete}
	if strategy := props[tnsapi.PropertyDeleteStrategy]; strategy != "" {
		o.deleteStrategy = strategy
	}
	return o, nil
}

// presentationPropertiesRecorded reports whether every key in keys has a non-empty value.
// DeleteVolume reads share/target/namespace IDs from these properties only, so a volume
// missing any of them would leak its presentation objects on delete.
func presentationPropertiesRecorded(props map[string]string, keys []string) bool {
	for _, k := range keys {
		if props[k] == "" {
			return false
		}
	}
	return true
}

// forgetPresentationIDs zeroes the given ID properties on a ZVOL once the objects they name
// are gone (deleted, or found to belong to something else). Failure is only logged: the
// ownership checks above keep a stale ID from reaching another volume on retry.
func (s *ControllerService) forgetPresentationIDs(ctx context.Context, datasetID string, keys ...string) {
	if datasetID == "" {
		return
	}
	props := make(map[string]string, len(keys))
	for _, k := range keys {
		props[k] = "0"
	}
	if err := s.apiClient.SetDatasetProperties(ctx, datasetID, props); err != nil && !tnsapi.IsNotFound(err) {
		klog.Warningf("Failed to clear presentation IDs on %s after deleting them: %v", datasetID, err)
	}
}

// Presentation objects (iSCSI target/extent, NVMe-oF namespace/subsystem) are deleted by
// the IDs recorded on the ZVOL. Those IDs outlive the objects whenever a delete attempt
// stops part-way: a later object fails, the property write that clears an ID fails, or the
// ZVOL delete fails. TrueNAS may hand a freed ID to another volume, so before deleting by
// a recorded ID the object is looked up and checked against this volume:
//   - gone: nothing to delete;
//   - belongs to something else: never touched;
//   - lookup failed: the attempt fails (Unavailable) rather than deleting blind.
// After each successful delete the ID property is cleared at once, so progress survives a
// failure of the next step. Clearing is best effort: the identity check is what keeps a
// stale ID from reaching another volume.

// presentationVolumeName is the CSI volume name that targets and subsystems are named
// after. meta.Name may be a dataset-path volume ID ("tank/csi/pvc-x"); CSI names never
// contain "/", so the last element is the name either way.
func presentationVolumeName(meta *VolumeMetadata) string {
	return path.Base(meta.Name)
}

// zvolDevice is the device a ZVOL is presented as ("zvol/<dataset>").
func zvolDevice(meta *VolumeMetadata) string {
	return "zvol/" + meta.DatasetID
}

// ownedISCSITarget reports whether target meta.ISCSITargetID exists and is this volume's:
// targets are named after the volume (the stored IQN ends in ":<target name>").
func (s *ControllerService) ownedISCSITarget(ctx context.Context, meta *VolumeMetadata) (bool, error) {
	if meta.ISCSITargetID == 0 {
		return false, nil
	}
	targets, err := s.apiClient.QueryISCSITargets(ctx, []interface{}{[]interface{}{"id", "=", meta.ISCSITargetID}})
	if err != nil {
		return false, fmt.Errorf("query iSCSI target %d: %w", meta.ISCSITargetID, err)
	}
	for _, t := range targets {
		if t.ID != meta.ISCSITargetID {
			continue
		}
		if t.Name == presentationVolumeName(meta) || (meta.ISCSIIQN != "" && strings.HasSuffix(meta.ISCSIIQN, ":"+t.Name)) {
			return true, nil
		}
		klog.Warningf("iSCSI target %d is now %q, not volume %s: leaving it alone", t.ID, t.Name, meta.Name)
		return false, nil
	}
	return false, nil
}

// ownedISCSIExtent reports whether extent meta.ISCSIExtentID exists and is backed by this ZVOL.
func (s *ControllerService) ownedISCSIExtent(ctx context.Context, meta *VolumeMetadata) (bool, error) {
	if meta.ISCSIExtentID == 0 {
		return false, nil
	}
	extents, err := s.apiClient.QueryISCSIExtents(ctx, []interface{}{[]interface{}{"id", "=", meta.ISCSIExtentID}})
	if err != nil {
		return false, fmt.Errorf("query iSCSI extent %d: %w", meta.ISCSIExtentID, err)
	}
	for _, e := range extents {
		if e.ID != meta.ISCSIExtentID {
			continue
		}
		if e.Disk == zvolDevice(meta) {
			return true, nil
		}
		klog.Warningf("iSCSI extent %d now backs %s, not %s: leaving it alone", e.ID, e.Disk, zvolDevice(meta))
		return false, nil
	}
	return false, nil
}

// ownedNVMeOFNamespace reports whether namespace meta.NVMeOFNamespaceID exists and is
// backed by this ZVOL.
func (s *ControllerService) ownedNVMeOFNamespace(ctx context.Context, meta *VolumeMetadata) (bool, error) {
	if meta.NVMeOFNamespaceID <= 0 {
		return false, nil
	}
	ns, err := s.apiClient.QueryNVMeOFNamespaceByID(ctx, meta.NVMeOFNamespaceID)
	if err != nil {
		return false, fmt.Errorf("query NVMe-oF namespace %d: %w", meta.NVMeOFNamespaceID, err)
	}
	if ns == nil {
		return false, nil
	}
	if strings.TrimPrefix(ns.GetDevice(), "/dev/") == zvolDevice(meta) {
		return true, nil
	}
	klog.Warningf("NVMe-oF namespace %d now backs %s, not %s: leaving it alone", ns.ID, ns.GetDevice(), zvolDevice(meta))
	return false, nil
}

// ownedNVMeOFSubsystem reports whether subsystem meta.NVMeOFSubsystemID exists and is this
// volume's: it carries the stored NQN, or (no NQN stored) its name ends in ":<volume>".
func (s *ControllerService) ownedNVMeOFSubsystem(ctx context.Context, meta *VolumeMetadata) (bool, error) {
	if meta.NVMeOFSubsystemID <= 0 {
		return false, nil
	}
	subs, err := s.apiClient.QueryNVMeOFSubsystems(ctx, []interface{}{[]interface{}{"id", "=", meta.NVMeOFSubsystemID}})
	if err != nil {
		return false, fmt.Errorf("query NVMe-oF subsystem %d: %w", meta.NVMeOFSubsystemID, err)
	}
	for _, sub := range subs {
		if sub.ID != meta.NVMeOFSubsystemID {
			continue
		}
		if meta.NVMeOFNQN != "" && (sub.NQN == meta.NVMeOFNQN || sub.Name == meta.NVMeOFNQN) {
			return true, nil
		}
		if meta.NVMeOFNQN == "" && strings.HasSuffix(sub.Name, ":"+presentationVolumeName(meta)) {
			return true, nil
		}
		klog.Warningf("NVMe-oF subsystem %d is now %q, not volume %s: leaving it alone", sub.ID, sub.NQN, meta.Name)
		return false, nil
	}
	return false, nil
}

// presentationLookupFailed wraps a failed identity lookup: the attempt stops, the ZVOL and
// its properties stay, and the CO retries.
func presentationLookupFailed(meta *VolumeMetadata, err error) error {
	return status.Errorf(codes.Unavailable, "cannot verify presentation objects of volume %s before deleting them: %v", meta.Name, err)
}
