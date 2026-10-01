package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"hupi/internal/ingest"
	"hupi/internal/metrics"
	"hupi/internal/provider"
)

const (
	// maxAttachmentBytes bounds a single attachment's raw size, checked
	// after base64 decoding and before any parsing/upstream call — a
	// hard request-level rejection (400), not a degrade, since exceeding
	// it is a malformed/abusive request rather than a file that merely
	// failed to extract cleanly.
	maxAttachmentBytes = 8 << 20 // 8 MiB

	// maxAttachmentsPerRequest bounds attachment count — also a hard
	// 400, enforced before any parsing starts.
	maxAttachmentsPerRequest = 5

	// imageDescribeTimeout bounds one DescribeImage call — a real extra
	// network call per image, degraded to a placeholder rather than
	// blocking the turn indefinitely on a slow/hung vision provider.
	imageDescribeTimeout = 20 * time.Second

	attachmentKindDocument = "document"
	attachmentKindImage    = "image"
)

// allowedImageMIME is checked before any upstream vision call — a
// client-declared MIME outside this set is rejected (400) the same way
// an invalid message role already is, rather than forwarded to the
// vision provider.
var allowedImageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

var imageDescribeInstruction = "Describe this image factually and specifically: what it shows, any visible text, notable objects, people, or scenes. Be concise but concrete enough that someone who can't see the image would understand what's in it."

// mergeAttachments is the one merge point both file documents and
// images go through — see the file-ingestion design: each attachment is
// converted to plain text once, here (extracted document text, or a
// vision-model image caption), then appended to the last user message's
// content with a provenance marker. Because this mutates the same
// []provider.Message already threaded through the rest of the request,
// buildEpisode, Capture, EpisodeEmbedText, and the existing per-episode
// chunking all apply with no further code changes — file/image content
// becomes "just more characters in the one captured input string."
//
// Returns the (possibly mutated) messages slice, a list of non-fatal
// warnings (one per attachment that didn't extract/caption cleanly, for
// the response's AttachmentWarnings field), and an error only for a
// genuine request-level problem — validation failures (too many
// attachments, oversized, bad MIME, no user message to attach to) map to
// 400; a per-attachment extraction/captioning failure never does,
// degrading to a placeholder marker instead (same "never block the
// turn" posture as AggregationHint/extractPerEpisodeFacts).
func (h *Handler) mergeAttachments(ctx context.Context, messages []provider.Message, atts []chatAttachment) ([]provider.Message, []string, error) {
	if len(atts) == 0 {
		return messages, nil, nil
	}
	if len(atts) > maxAttachmentsPerRequest {
		return nil, nil, fmt.Errorf("too many attachments: %d (max %d)", len(atts), maxAttachmentsPerRequest)
	}

	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleUser {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return nil, nil, fmt.Errorf("attachments require at least one user message")
	}

	decoded := make([][]byte, len(atts))
	for i, att := range atts {
		switch att.Type {
		case attachmentKindDocument, attachmentKindImage:
		default:
			return nil, nil, fmt.Errorf("attachment %d: invalid type %q: must be %q or %q", i, att.Type, attachmentKindDocument, attachmentKindImage)
		}
		data, err := base64.StdEncoding.DecodeString(att.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("attachment %d: invalid base64 data: %w", i, err)
		}
		if len(data) > maxAttachmentBytes {
			return nil, nil, fmt.Errorf("attachment %d: exceeds %d byte size limit", i, maxAttachmentBytes)
		}
		if att.Type == attachmentKindImage && !allowedImageMIME[att.ContentType] {
			return nil, nil, fmt.Errorf("attachment %d: content_type %q is not an allowed image type", i, att.ContentType)
		}
		decoded[i] = data
	}

	type blockResult struct {
		block   string
		warning string
	}
	results := make([]blockResult, len(atts))
	var wg sync.WaitGroup
	for i, att := range atts {
		i, att, data := i, att, decoded[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			block, warning := h.processAttachment(ctx, att, data)
			results[i] = blockResult{block: block, warning: warning}
		}()
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString(messages[lastUserIdx].Content)
	var warnings []string
	for _, r := range results {
		sb.WriteString("\n\n")
		sb.WriteString(r.block)
		if r.warning != "" {
			warnings = append(warnings, r.warning)
		}
	}
	messages[lastUserIdx].Content = sb.String()
	return messages, warnings, nil
}

// processAttachment dispatches one already-validated attachment to
// document extraction or image captioning, returning the
// provenance-framed block to append and an optional warning — never an
// error, since a single attachment's extraction/captioning failure must
// degrade, not fail the whole request.
func (h *Handler) processAttachment(ctx context.Context, att chatAttachment, data []byte) (block string, warning string) {
	start := time.Now()
	defer func() {
		metrics.AttachmentIngestDuration.WithLabelValues(att.Type).Observe(time.Since(start).Seconds())
	}()

	filename := att.Filename
	if filename == "" {
		filename = "attachment"
	}

	switch att.Type {
	case attachmentKindImage:
		return h.describeImageAttachment(ctx, filename, att.ContentType, data)
	default:
		return h.extractDocumentAttachment(ctx, filename, att.ContentType, data)
	}
}

func (h *Handler) extractDocumentAttachment(ctx context.Context, filename, contentType string, data []byte) (block string, warning string) {
	res, err := ingest.Extract(ctx, ingest.Attachment{Filename: filename, ContentType: contentType, Data: data})
	if err != nil {
		h.log().Warn("attachment: document extraction failed", "filename", filename, "error", err)
		metrics.AttachmentIngestTotal.WithLabelValues(attachmentKindDocument, "error").Inc()
		warning = fmt.Sprintf("%s: could not be read (%v)", filename, err)
		return fmt.Sprintf("[Attached file: %s — could not be read]", filename), warning
	}
	if res.Warning != "" {
		metrics.AttachmentIngestTotal.WithLabelValues(attachmentKindDocument, "warning").Inc()
		warning = fmt.Sprintf("%s: %s", filename, res.Warning)
	} else {
		metrics.AttachmentIngestTotal.WithLabelValues(attachmentKindDocument, "ok").Inc()
	}
	return fmt.Sprintf("[Attached file: %s]\n%s\n[End of attached file: %s]", filename, res.Text, filename), warning
}

func (h *Handler) describeImageAttachment(ctx context.Context, filename, mimeType string, data []byte) (block string, warning string) {
	vp, ok := h.Registry.Vision().(provider.VisionCapable)
	if !ok {
		metrics.AttachmentIngestTotal.WithLabelValues(attachmentKindImage, "error").Inc()
		warning = fmt.Sprintf("%s: the configured vision provider does not support image description", filename)
		return fmt.Sprintf("[Shared image: %s — description unavailable]", filename), warning
	}

	describeCtx, cancel := context.WithTimeout(ctx, imageDescribeTimeout)
	defer cancel()
	caption, err := vp.DescribeImage(describeCtx, provider.ImageInput{Data: data, MIMEType: mimeType}, imageDescribeInstruction)
	if err != nil {
		h.log().Warn("attachment: image captioning failed", "filename", filename, "error", err)
		metrics.AttachmentIngestTotal.WithLabelValues(attachmentKindImage, "error").Inc()
		warning = fmt.Sprintf("%s: description unavailable (%v)", filename, err)
		return fmt.Sprintf("[Shared image: %s — description unavailable]", filename), warning
	}
	metrics.AttachmentIngestTotal.WithLabelValues(attachmentKindImage, "ok").Inc()
	return fmt.Sprintf("[Shared image: %s]\n%s", filename, caption), ""
}
