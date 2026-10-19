package nvmeof

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/fenio/tns-csi/tests/e2e/framework"
)

// A ZFS clone or snapshot restore of an XFS ZVOL keeps the source filesystem's
// UUID. The kernel refuses to mount a second XFS filesystem with a UUID that is
// already mounted on the node, so these copies are mounted while the source
// POD is still running on the same node.
var _ = Describe("NVMe-oF XFS Clone And Restore", func() {
	var f *framework.Framework
	var ctx context.Context

	const (
		storageClassName  = "tns-csi-nvmeof-xfs-clone"
		snapshotClassName = "tns-csi-nvmeof-xfs-snapshot"
		sourcePVCName     = "test-pvc-nvmeof-xfs-source"
		sourcePodName     = "test-pod-nvmeof-xfs-source"
		snapshotName      = "test-snap-nvmeof-xfs-source"
		payloadPath       = volumeMountPath + "/payload.bin"
		copyOnlyPath      = volumeMountPath + "/copy-only.bin"
		payloadSize       = 8 << 20
		podTimeout        = 360 * time.Second
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		f, err = framework.NewFramework()
		Expect(err).NotTo(HaveOccurred())
		err = f.Setup("nvmeof")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if f != nil {
			f.Teardown()
		}
	})

	It("should mount an XFS clone and snapshot restore while the source stays mounted", func() {
		rwo := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}

		By("Creating StorageClass with fsType xfs")
		err := f.K8s.CreateStorageClassWithParams(ctx, storageClassName, "tns.csi.io", map[string]string{
			"protocol":  "nvmeof",
			"server":    f.Config.TrueNASHost,
			"pool":      f.Config.TrueNASPool,
			"transport": "tcp",
			"port":      "4420",
			"fsType":    "xfs",
		})
		Expect(err).NotTo(HaveOccurred())
		f.Cleanup.Add(func() error {
			return f.K8s.DeleteStorageClass(context.Background(), storageClassName)
		})

		By("Creating VolumeSnapshotClass")
		err = f.K8s.CreateVolumeSnapshotClass(ctx, snapshotClassName, "tns.csi.io", "Delete")
		Expect(err).NotTo(HaveOccurred())
		f.Cleanup.Add(func() error {
			return f.K8s.DeleteVolumeSnapshotClass(context.Background(), snapshotClassName)
		})

		By("Creating source PVC and POD")
		_, err = f.CreatePVC(ctx, framework.PVCOptions{
			Name:             sourcePVCName,
			StorageClassName: storageClassName,
			Size:             "1Gi",
			AccessModes:      rwo,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = f.CreatePod(ctx, framework.PodOptions{
			Name:      sourcePodName,
			PVCName:   sourcePVCName,
			MountPath: volumeMountPath,
		})
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPodReady(ctx, sourcePodName, podTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Writing a random payload on the mounted source")
		source := podRunner(f, sourcePodName)
		written, err := writeFilePayload(ctx, source, payloadPath, payloadSize)
		Expect(err).NotTo(HaveOccurred())

		By("Cloning the mounted source")
		const clonePVCName = "test-pvc-nvmeof-xfs-clone"
		err = f.K8s.CreatePVCFromPVC(ctx, clonePVCName, sourcePVCName, storageClassName, "1Gi", rwo)
		Expect(err).NotTo(HaveOccurred())
		f.RegisterPVCCleanup(clonePVCName)

		By("Snapshotting the mounted source and restoring the snapshot")
		err = f.K8s.CreateVolumeSnapshot(ctx, snapshotName, sourcePVCName, snapshotClassName)
		Expect(err).NotTo(HaveOccurred())
		f.Cleanup.Add(func() error {
			return f.K8s.DeleteVolumeSnapshot(context.Background(), snapshotName)
		})
		err = f.K8s.WaitForSnapshotReady(ctx, snapshotName, 3*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		const restorePVCName = "test-pvc-nvmeof-xfs-restore"
		err = f.K8s.CreatePVCFromSnapshot(ctx, restorePVCName, snapshotName, storageClassName, "1Gi", rwo)
		Expect(err).NotTo(HaveOccurred())
		f.RegisterPVCCleanup(restorePVCName)

		copies := []struct{ pvcName, podName string }{
			{clonePVCName, "test-pod-nvmeof-xfs-clone"},
			{restorePVCName, "test-pod-nvmeof-xfs-restore"},
		}
		for _, copied := range copies {
			By("Mounting " + copied.pvcName + " while the source POD is running")
			_, err = f.CreatePod(ctx, framework.PodOptions{
				Name:      copied.podName,
				PVCName:   copied.pvcName,
				MountPath: volumeMountPath,
			})
			Expect(err).NotTo(HaveOccurred())
			err = f.K8s.WaitForPodReady(ctx, copied.podName, podTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying " + copied.pvcName + " is XFS with the source payload")
			run := podRunner(f, copied.podName)
			fsType, fsErr := volumeFSType(ctx, run)
			Expect(fsErr).NotTo(HaveOccurred())
			Expect(fsType).To(Equal("xfs"))
			err = verifyFilePayload(ctx, run, payloadPath, written)
			Expect(err).NotTo(HaveOccurred())

			By("Writing to " + copied.pvcName + " without changing the source")
			_, err = writeFilePayload(ctx, run, copyOnlyPath, 4<<10)
			Expect(err).NotTo(HaveOccurred())
			err = verifyFilePayload(ctx, source, payloadPath, written)
			Expect(err).NotTo(HaveOccurred())
			exists, existsErr := f.K8s.FileExistsInPod(ctx, sourcePodName, copyOnlyPath)
			Expect(existsErr).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		}
	})
})
