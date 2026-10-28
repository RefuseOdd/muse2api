package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/muse2api/muse2api/internal/engine"
)

// prepareReferenceImages validates and snapshots every reference before a task
// is accepted or a generation begins. The returned values are canonical data
// URLs, preserving the caller's order.
func prepareReferenceImages(eng *engine.Engine, images []string) ([]string, error) {
	return prepareReferences(context.Background(), eng, images)
}
func prepareReferences(ctx context.Context, eng *engine.Engine, images []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	prepared := make([]string, 0, len(images))
	total := 0
	for index, image := range images {
		b64, mimeType, err := eng.NormalizeImageContext(ctx, image)
		if err != nil {
			return nil, httpError(400, "images[%d]: %v", index, err)
		}
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, httpError(400, "images[%d]: invalid base64 payload", index)
		}
		total += len(data)
		if total > maxReferenceTotalBytes {
			return nil, httpError(400, "reference images exceed the 24 MiB total limit")
		}
		prepared = append(prepared, fmt.Sprintf("data:%s;base64,%s", mimeType, b64))
	}
	return prepared, nil
}
