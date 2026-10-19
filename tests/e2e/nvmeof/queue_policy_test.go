package nvmeof

import (
	"context"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/fenio/tns-csi/tests/e2e/framework"
)

// The requested values differ from kernel defaults so that observing them
// shows the driver applied them.
var _ = Describe("NVMe-oF Queue Policy", func() {
	var f *framework.Framework
	var ctx context.Context

	const (
		storageClassName     = "tns-csi-nvmeof-queue-policy"
		pvcName              = "test-pvc-nvmeof-queue-policy"
		firstPodName         = "test-pod-nvmeof-queue-policy-first"
		secondPodName        = "test-pod-nvmeof-queue-policy-second"
		payloadPath          = volumeMountPath + "/payload.bin"
		payloadSize          = 8 << 20
		requestedCapKB       = 512
		requestedReadAheadKB = 256
		podTimeout           = 360 * time.Second
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

	// The cap is the minimum of the requested, current and hardware caps,
	// aligned to the logical block size, so only an upper bound is exact.
	expectPolicy := func(transition string, limits queueLimits) {
		GinkgoWriter.Printf("%s: device %s max_sectors_kb=%d max_hw_sectors_kb=%d read_ahead_kb=%d logical_block_size=%d\n",
			transition, limits.device, limits.maxSectorsKB, limits.maxHWSectorsKB, limits.readAheadKB, limits.logicalBlockSize)
		Expect(limits.logicalBlockSize).To(BeNumerically(">", 0), transition)
		Expect(limits.maxSectorsKB).To(BeNumerically(">", 0), transition)
		Expect(limits.maxSectorsKB).To(BeNumerically("<=", requestedCapKB), transition)
		Expect(limits.maxSectorsKB).To(BeNumerically("<=", limits.maxHWSectorsKB), transition)
		Expect(limits.maxSectorsKB*1024%limits.logicalBlockSize).To(BeZero(), transition)
		Expect(limits.readAheadKB).To(Equal(int64(requestedReadAheadKB)), transition)
	}

	It("should keep the requested transfer cap and readahead across expansion and restage", func() {
		By("Creating StorageClass with an explicit transfer cap and readahead")
		err := f.K8s.CreateStorageClassWithParams(ctx, storageClassName, "tns.csi.io", map[string]string{
			"protocol":              "nvmeof",
			"server":                f.Config.TrueNASHost,
			"pool":                  f.Config.TrueNASPool,
			"transport":             "tcp",
			"port":                  "4420",
			"fsType":                "xfs",
			"nvmeof.max-sectors-kb": strconv.Itoa(requestedCapKB),
			"nvmeof.read-ahead-kb":  strconv.Itoa(requestedReadAheadKB),
		})
		Expect(err).NotTo(HaveOccurred())
		f.Cleanup.Add(func() error {
			return f.K8s.DeleteStorageClass(context.Background(), storageClassName)
		})

		By("Creating PVC and the first POD")
		_, err = f.CreatePVC(ctx, framework.PVCOptions{
			Name:             pvcName,
			StorageClassName: storageClassName,
			Size:             "1Gi",
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = f.CreatePod(ctx, framework.PodOptions{
			Name:      firstPodName,
			PVCName:   pvcName,
			MountPath: volumeMountPath,
		})
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPodReady(ctx, firstPodName, podTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Checking the queue policy after the first stage")
		first := podRunner(f, firstPodName)
		limits, err := volumeQueueLimits(ctx, first)
		Expect(err).NotTo(HaveOccurred())
		expectPolicy("first stage", limits)
		written, err := writeFilePayload(ctx, first, payloadPath, payloadSize)
		Expect(err).NotTo(HaveOccurred())

		By("Expanding the mounted PVC to 2Gi")
		err = f.K8s.ExpandPVC(ctx, pvcName, "2Gi")
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPVCCapacity(ctx, pvcName, "2Gi", 5*time.Minute)
		Expect(err).NotTo(HaveOccurred())

		By("Checking the queue policy after expansion")
		limits, err = volumeQueueLimits(ctx, first)
		Expect(err).NotTo(HaveOccurred())
		expectPolicy("after expansion", limits)

		By("Restaging the volume in a new POD")
		err = f.K8s.DeletePod(ctx, firstPodName)
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPodDeleted(ctx, firstPodName, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		_, err = f.CreatePod(ctx, framework.PodOptions{
			Name:      secondPodName,
			PVCName:   pvcName,
			MountPath: volumeMountPath,
		})
		Expect(err).NotTo(HaveOccurred())
		err = f.K8s.WaitForPodReady(ctx, secondPodName, podTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Checking the queue policy and data after restage")
		second := podRunner(f, secondPodName)
		limits, err = volumeQueueLimits(ctx, second)
		Expect(err).NotTo(HaveOccurred())
		expectPolicy("after restage", limits)
		err = verifyFilePayload(ctx, second, payloadPath, written)
		Expect(err).NotTo(HaveOccurred())
	})
})
