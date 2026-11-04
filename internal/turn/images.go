package turn

import (
	"context"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
)

func (e *Executor) loadCurrentTurnImages(
	ctx context.Context, userID, sessionID, content string, refs []model.MessageAttachmentRef,
) ([]agent.MessageImage, string) {
	images := make([]agent.MessageImage, 0, len(refs))
	var missing []string
	for _, ref := range refs {
		ext, ok := model.AttachmentMIMEExt[ref.MIME]
		if !ok {
			missing = append(missing, ref.Filename)
			continue
		}
		path := filepath.Join(e.dataDir, userID, "attachments", sessionID, ref.ID+ext)
		data, err := os.ReadFile(path)
		if err != nil {
			logger.FromContext(ctx).Error("current-turn attachment read failed; degrading to placeholder",
				zap.String("op", "turn.start"),
				zap.String("attachment_id", ref.ID),
				zap.String("path", path),
				zap.Error(err))
			missing = append(missing, ref.Filename)
			continue
		}
		images = append(images, agent.MessageImage{AttachmentID: ref.ID, Filename: ref.Filename, MIMEType: ref.MIME, Data: data})
	}
	return images, RenderAttachmentPlaceholders(content, missing)
}
