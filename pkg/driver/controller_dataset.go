package driver

import (
	"context"

	"github.com/fenio/tns-csi/pkg/tnsapi"
)

// TrueNAS query-filter field names used by the driver's specific lookups.
const (
	queryFieldPath = "path" // sharing.nfs.query / sharing.smb.query share path
	queryFieldDisk = "disk" // iscsi.extent.query backing device ("zvol/<dataset>")
)

// queryExactDataset looks up one dataset by exact ID and returns it as a one-element
// slice, or an empty slice when it does not exist. It used to filter an "id starts with"
// listing, which returned the dataset's whole subtree. The ID is still re-checked so
// prefix siblings and child datasets are never adopted as the requested volume.
func (s *ControllerService) queryExactDataset(ctx context.Context, datasetID string) ([]tnsapi.Dataset, error) {
	dataset, err := s.apiClient.Dataset(ctx, datasetID)
	if tnsapi.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if dataset == nil || dataset.ID != datasetID {
		return nil, nil
	}
	return []tnsapi.Dataset{*dataset}, nil
}
