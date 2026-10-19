package nvmeof

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/fenio/tns-csi/tests/e2e/framework"
)

// Copies of an XFS volume keep its filesystem UUID, so each copy here is
// mounted only after every other mount of that filesystem on the node is gone.
var _ = Describe("NVMe-oF XFS Lifecycle", func() {
	var f *framework.Framework
	var ctx context.Context

	const (
		storageClassName  = "tns-csi-nvmeof-xfs-lifecycle"
		snapshotClassName = "tns-csi-nvmeof-xfs-lifecycle-snapshot"
		sourcePVCName     = "test-pvc-nvmeof-xfs-grow"
		sourcePodName     = "test-pod-nvmeof-xfs-grow"
		snapshotName      = "test-snap-nvmeof-xfs-grow"
		beforeGrowPath    = volumeMountPath + "/before-grow.bin"
		afterGrowPath     = volumeMountPath + "/after-grow.bin"
		payloadSize       = 8 << 20
		originalSize      = "1Gi"
		grownSize         = "2Gi"
		originalBytes     = 1 << 30
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

	It("should grow XFS online and restore or clone it after the source is unmounted", func() {
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

		By("Creating a " + originalSize + " source PVC and POD")
		_, err = f.CreatePVC(ctx, framework.PVCOptions{
			Name:             sourcePVCName,
			StorageClassName: storageClassName,
			Size:             originalSize,
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

		source := podRunner(f, sourcePodName)
		fsType, err := volumeFSType(ctx, source)
		Expect(err).NotTo(HaveOccurred())
		Expect(fsType).To(Equal("xfs"))
		total, _, err := volumeBytes(ctx, source)
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(BeNumerically("<=", originalBytes))

		By("Writing a payload before growing")
		beforeGrow, err := writeFilePayload(ctx, source, beforeGrowPath, payloadSize)
		Expect(err).NotTo(HaveOccurred())

		By("Expanding the mounted PVC to " + grownSize)
		err = f.K8s.ExpandPVC(ctx, sourcePVCName, grownSize)
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPVCCapacity(ctx, sourcePVCName, grownSize, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred())

		// A filesystem cannot exceed its device, so exceeding the original
		// device size proves the node grew the mounted XFS.
		By("Waiting for the mounted XFS to exceed the original device size")
		Eventually(func() (int64, error) {
			grown, _, statErr := volumeBytes(ctx, source)
			return grown, statErr
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeNumerically(">", originalBytes))
		_, available, err := volumeBytes(ctx, source)
		Expect(err).NotTo(HaveOccurred())
		Expect(available).To(BeNumerically(">", originalBytes))

		By("Verifying the earlier payload and writing another after growing")
		err = verifyFilePayload(ctx, source, beforeGrowPath, beforeGrow)
		Expect(err).NotTo(HaveOccurred())
		afterGrow, err := writeFilePayload(ctx, source, afterGrowPath, payloadSize)
		Expect(err).NotTo(HaveOccurred())

		By("Snapshotting the grown source")
		err = f.K8s.CreateVolumeSnapshot(ctx, snapshotName, sourcePVCName, snapshotClassName)
		Expect(err).NotTo(HaveOccurred())
		f.Cleanup.Add(func() error {
			return f.K8s.DeleteVolumeSnapshot(context.Background(), snapshotName)
		})
		err = f.K8s.WaitForSnapshotReady(ctx, snapshotName, 3*time.Minute)
		Expect(err).NotTo(HaveOccurred())

		By("Deleting the source POD so the source XFS is unmounted")
		err = f.K8s.DeletePod(ctx, sourcePodName)
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPodDeleted(ctx, sourcePodName, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred())

		copies := []struct {
			create           func(name string) error
			pvcName, podName string
		}{
			{
				pvcName: "test-pvc-nvmeof-xfs-grow-restore",
				podName: "test-pod-nvmeof-xfs-grow-restore",
				create: func(name string) error {
					return f.K8s.CreatePVCFromSnapshot(ctx, name, snapshotName, storageClassName, grownSize, rwo)
				},
			},
			{
				pvcName: "test-pvc-nvmeof-xfs-grow-clone",
				podName: "test-pod-nvmeof-xfs-grow-clone",
				create: func(name string) error {
					return f.K8s.CreatePVCFromPVC(ctx, name, sourcePVCName, storageClassName, grownSize, rwo)
				},
			},
		}
		for _, copied := range copies {
			By("Creating " + copied.pvcName)
			err = copied.create(copied.pvcName)
			Expect(err).NotTo(HaveOccurred())
			f.RegisterPVCCleanup(copied.pvcName)
			_, err = f.CreatePod(ctx, framework.PodOptions{
				Name:      copied.podName,
				PVCName:   copied.pvcName,
				MountPath: volumeMountPath,
			})
			Expect(err).NotTo(HaveOccurred())
			err = f.K8s.WaitForPodReady(ctx, copied.podName, podTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying " + copied.pvcName + " is the grown XFS with both payloads")
			run := podRunner(f, copied.podName)
			fsType, err = volumeFSType(ctx, run)
			Expect(err).NotTo(HaveOccurred())
			Expect(fsType).To(Equal("xfs"))
			total, _, err = volumeBytes(ctx, run)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">", originalBytes))
			err = verifyFilePayload(ctx, run, beforeGrowPath, beforeGrow)
			Expect(err).NotTo(HaveOccurred())
			err = verifyFilePayload(ctx, run, afterGrowPath, afterGrow)
			Expect(err).NotTo(HaveOccurred())

			By("Unmounting " + copied.pvcName + " before the next copy")
			err = f.K8s.DeletePod(ctx, copied.podName)
			Expect(err).NotTo(HaveOccurred())
			err = f.K8s.WaitForPodDeleted(ctx, copied.podName, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred())
		}
	})
})
