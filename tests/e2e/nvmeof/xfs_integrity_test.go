package nvmeof

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/fenio/tns-csi/tests/e2e/framework"
)

var _ = Describe("NVMe-oF XFS Integrity", func() {
	var f *framework.Framework
	var ctx context.Context

	const (
		storageClassName = "tns-csi-nvmeof-xfs-integrity"
		pvcName          = "test-pvc-nvmeof-xfs-integrity"
		writerPodName    = "test-pod-nvmeof-xfs-writer"
		readerPodName    = "test-pod-nvmeof-xfs-reader"
		mountPath        = "/data"
		podTimeout       = 360 * time.Second
	)

	// Application buffer sizes from 4 KiB through 32 MiB, each written once.
	payloadSizes := []int64{4 << 10, 128 << 10, 1 << 20, 4 << 20, 8 << 20, 16 << 20, 32 << 20}

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

	It("should preserve exact file contents on XFS across POD restarts", func() {
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

		By("Creating PVC")
		_, err = f.CreatePVC(ctx, framework.PVCOptions{
			Name:             pvcName,
			StorageClassName: storageClassName,
			Size:             "1Gi",
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		})
		Expect(err).NotTo(HaveOccurred())

		By("Creating writer POD")
		_, err = f.CreatePod(ctx, framework.PodOptions{
			Name:      writerPodName,
			PVCName:   pvcName,
			MountPath: mountPath,
		})
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPodReady(ctx, writerPodName, podTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying the volume is mounted as XFS")
		writer := podRunner(f, writerPodName)
		fsType, err := mountedFSType(ctx, writer, mountPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(fsType).To(Equal("xfs"))

		By("Writing finite random files with fsync")
		paths := make([]string, len(payloadSizes))
		written := make([]payloadDigest, len(payloadSizes))
		for i, size := range payloadSizes {
			paths[i] = fmt.Sprintf("%s/integrity-%d.bin", mountPath, size)
			written[i], err = writeFilePayload(ctx, writer, paths[i], size)
			Expect(err).NotTo(HaveOccurred())
		}

		By("Deleting writer POD so the volume is unmounted")
		err = f.K8s.DeletePod(ctx, writerPodName)
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPodDeleted(ctx, writerPodName, 120*time.Second)
		Expect(err).NotTo(HaveOccurred())

		By("Creating reader POD")
		_, err = f.CreatePod(ctx, framework.PodOptions{
			Name:      readerPodName,
			PVCName:   pvcName,
			MountPath: mountPath,
		})
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPodReady(ctx, readerPodName, podTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying the remounted volume is XFS")
		reader := podRunner(f, readerPodName)
		fsType, err = mountedFSType(ctx, reader, mountPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(fsType).To(Equal("xfs"))

		By("Verifying every file's exact length and SHA-256")
		for i, path := range paths {
			err = verifyFilePayload(ctx, reader, path, written[i])
			Expect(err).NotTo(HaveOccurred())
		}
	})
})
