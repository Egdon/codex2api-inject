package basispoints

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// AttachmentsURL is the Excel add-in's "Upload file" endpoint. BPS refuses
// inline images in messages but accepts the file IDs this endpoint returns.
const AttachmentsURL = "https://bps.openai.com/basispoints/api/attachments"

// maxInlineImageBytes bounds one decoded image before it is uploaded.
const maxInlineImageBytes = 20 << 20

// isInlineImage reports whether raw is a base64 data:image URL, the form
// Codex uses for pasted screenshots and view_image results.
func isInlineImage(raw string) bool {
	head := strings.ToLower(raw[:min(len(raw), 64)])
	return strings.HasPrefix(head, "data:image/") && strings.Contains(head, ";base64,")
}

// HTTPS references and inline data:image URLs are accepted here. Inline images
// are placed on the wire later by RewriteImages, which needs an uploader.
func validateImage(part object) error {
	raw, ok := part["image_url"].(string)
	if !ok || raw == "" {
		return fmt.Errorf("basispoints input_image requires an HTTPS or data:image image_url; file IDs are unsupported")
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "data:") {
		if !isInlineImage(raw) {
			return fmt.Errorf("basispoints inline image input must be a base64 data:image URL")
		}
	} else if parsed, err := url.Parse(raw); err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || strings.TrimSpace(raw) != raw {
		return fmt.Errorf("basispoints input_image requires an absolute HTTPS image URL without embedded credentials")
	}
	if fileID := text(part["file_id"]); fileID != "" {
		return fmt.Errorf("basispoints input_image does not support file_id; provide only an HTTPS image_url")
	}
	if detail, exists := part["detail"]; exists && detail != nil {
		switch text(detail) {
		case "auto", "low", "high":
		case "original":
			// Normalize the client's fidelity hint only; retain the exact image.
			part["detail"] = "high"
		default:
			return fmt.Errorf("basispoints image detail must be auto, low or high")
		}
	}
	return nil
}

// ImageMode selects how inline data:image parts reach the Excel wire.
type ImageMode int

const (
	// ImagesDefault uploads message images, which BPS refuses inline, and keeps
	// tool-result images inline as the add-in's own tools send them.
	ImagesDefault ImageMode = iota
	// ImagesUploadAll uploads every inline image, including tool results.
	ImagesUploadAll
	// ImagesOmit replaces every inline image with a text note.
	ImagesOmit
)

// InlineImage is one data:image URL awaiting upload. Digest identifies the
// exact URL text, so callers can reuse an earlier upload without decoding.
type InlineImage struct {
	Digest string
	url    string
}

// Decode returns the validated media type and image bytes.
func (i InlineImage) Decode() (string, []byte, error) {
	return decodeDataImage(i.url)
}

// Uploader stores one image and returns its OpenAI file ID.
type Uploader func(image InlineImage) (string, error)

// ImageReport describes how a rewritten body carries its images, so the
// caller can pick the next mode if BPS still refuses the body.
type ImageReport struct {
	Inline  bool
	FileIDs []string
	// UploadErr is the first upload failure; the affected images were omitted.
	UploadErr error
	// InputErr is the first image that could not be used at all (bad media
	// type, size or encoding). It affects only that image; later images are
	// still uploaded.
	InputErr error
	// CurrentUploadErr is set when an image of the latest user turn was not
	// uploaded, for either reason. Callers should fail the request: the user
	// expects that image to be seen. Older history images degrade to notes so
	// a conversation is never stuck on an image from an earlier turn.
	CurrentUploadErr error
}

// ErrInvalidImage marks an inline image that cannot be used at all, as
// opposed to an upload endpoint failure. Its messages name only the problem,
// never image content, so callers may return them to the client.
var ErrInvalidImage = errors.New("invalid image input")

// Any reports whether the body still sends any image content upstream.
func (r ImageReport) Any() bool {
	return r.Inline || len(r.FileIDs) > 0
}

// decodeDataImage validates a base64 data:image URL and returns its lowercase
// media type and decoded bytes, accepting padded or unpadded base64 up to
// maxInlineImageBytes.
func decodeDataImage(raw string) (string, []byte, error) {
	header, payload, ok := strings.Cut(raw, ",")
	if !ok || !isInlineImage(raw) {
		return "", nil, fmt.Errorf("%w: not a base64 data:image URL", ErrInvalidImage)
	}
	mediaType, _, _ := strings.Cut(strings.TrimPrefix(header, "data:"), ";")
	mediaType = strings.ToLower(mediaType)
	// The media type becomes a multipart header on upload; anything beyond a
	// plain token (CR/LF, quotes, parameters) could inject headers.
	if !imageMediaType.MatchString(mediaType) {
		return "", nil, fmt.Errorf("%w: unsupported image media type", ErrInvalidImage)
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > maxInlineImageBytes {
		return "", nil, fmt.Errorf("%w: image exceeds %d MiB", ErrInvalidImage, maxInlineImageBytes>>20)
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
	}
	if err != nil || len(data) == 0 {
		return "", nil, fmt.Errorf("%w: invalid base64 image data", ErrInvalidImage)
	}
	return mediaType, data, nil
}

var imageMediaType = regexp.MustCompile(`^image/[a-z0-9][a-z0-9.+-]{0,63}$`)

func omittedImage(reason string) object {
	return object{"type": "input_text", "text": "[image content omitted: " + reason + "]"}
}

// RewriteImages places the inline images of a prepared BPS body according to
// mode. Bodies without inline images are returned unchanged. Task and turn
// metadata were derived by Prepare, so they do not depend on the mode.
func RewriteImages(body []byte, mode ImageMode, upload Uploader) ([]byte, ImageReport, error) {
	var report ImageReport
	if !bytes.Contains(body, []byte("data:image/")) {
		return body, report, nil
	}
	var source object
	if err := decode(body, &source); err != nil {
		return nil, report, fmt.Errorf("invalid prepared Basispoints body")
	}
	items, _ := source["input"].([]any)
	lastUser := -1
	for i, raw := range items {
		if item, _ := raw.(object); text(item["type"]) == "message" && text(item["role"]) == "user" {
			lastUser = i
		}
	}
	for i, raw := range items {
		item, _ := raw.(object)
		current := i >= lastUser
		switch text(item["type"]) {
		case "message":
			if content, ok := item["content"].([]any); ok {
				rewriteImageParts(content, mode, true, current, upload, &report)
			}
		case "function_call_output":
			if output, ok := item["output"].([]any); ok {
				rewriteImageParts(output, mode, mode == ImagesUploadAll, current, upload, &report)
			}
		}
	}
	out, err := json.Marshal(source)
	return out, report, err
}

// rewriteImageParts rewrites the inline images of one content array in place:
// omitted as a note in ImagesOmit, left inline when uploadInline is false, or
// uploaded and replaced by file_id. An invalid image becomes a note on its
// own; after the first upload endpoint failure the rest of the images become
// notes. report records what the body now carries; current marks parts of the
// latest user turn.
func rewriteImageParts(parts []any, mode ImageMode, uploadInline, current bool, upload Uploader, report *ImageReport) {
	for i, raw := range parts {
		part, _ := raw.(object)
		ref := text(part["image_url"])
		if text(part["type"]) != "input_image" || !isInlineImage(ref) {
			continue
		}
		switch {
		case mode == ImagesOmit:
			parts[i] = omittedImage("Basispoints did not accept it")
		case !uploadInline:
			report.Inline = true
		case report.UploadErr != nil:
			// One failed upload usually means the endpoint is unavailable for
			// this request; do not retry it for every remaining image.
			parts[i] = omittedImage("it could not be uploaded")
			if current && report.CurrentUploadErr == nil {
				report.CurrentUploadErr = report.UploadErr
			}
		default:
			fileID, err := uploadImage(ref, upload)
			if errors.Is(err, ErrInvalidImage) {
				// A bad image says nothing about the upload endpoint; keep
				// uploading the others.
				if report.InputErr == nil {
					report.InputErr = err
				}
				if current && report.CurrentUploadErr == nil {
					report.CurrentUploadErr = err
				}
				parts[i] = omittedImage("the image data is invalid")
				continue
			}
			if err != nil {
				report.UploadErr = err
				if current && report.CurrentUploadErr == nil {
					report.CurrentUploadErr = err
				}
				parts[i] = omittedImage("it could not be uploaded")
				continue
			}
			replaced := object{"type": "input_image", "file_id": fileID, "detail": "auto"}
			if detail := text(part["detail"]); detail != "" {
				replaced["detail"] = detail
			}
			parts[i] = replaced
			report.FileIDs = append(report.FileIDs, fileID)
		}
	}
}

// uploadImage hands one data URL to the caller's uploader, keyed by the
// SHA-256 of the URL text so a cached upload skips base64 decoding.
func uploadImage(ref string, upload Uploader) (string, error) {
	if upload == nil {
		return "", fmt.Errorf("no image uploader")
	}
	sum := sha256.Sum256([]byte(ref))
	fileID, err := upload(InlineImage{Digest: hex.EncodeToString(sum[:]), url: ref})
	if err != nil {
		return "", err
	}
	if fileID = strings.TrimSpace(fileID); fileID == "" {
		return "", fmt.Errorf("upload returned no file ID")
	}
	return fileID, nil
}
