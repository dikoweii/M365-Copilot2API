package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type imageEditTestFile struct {
	field string
	name  string
	data  []byte
}

func newImageEditMultipartRequest(t *testing.T, prompt string, files []imageEditTestFile) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if prompt != "" {
		if err := writer.WriteField("prompt", prompt); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		part, err := writer.CreateFormFile(file.field, file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

func parseImageEditTestForm(t *testing.T, files []imageEditTestFile) *multipart.Form {
	t.Helper()
	r := newImageEditMultipartRequest(t, "edit these", files)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.MultipartForm.RemoveAll()
	})
	return r.MultipartForm
}

func imageEditTestBytes(contentType string) []byte {
	switch contentType {
	case "image/png":
		return []byte("\x89PNG\r\n\x1a\nimage")
	case "image/jpeg":
		return []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}
	case "image/webp":
		return []byte("RIFF\x0c\x00\x00\x00WEBPVP8 image")
	default:
		return []byte("not an image")
	}
}

func assertInvalidImageEditResponse(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want %d body=%s", w.Code, status, w.Body.String())
	}
	var response struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, w.Body.String())
	}
	if response.Error.Type != "invalid_request_error" {
		t.Fatalf("error type=%q want invalid_request_error", response.Error.Type)
	}
}

func TestImageEditsValidation(t *testing.T) {
	t.Run("method", func(t *testing.T) {
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, httptest.NewRequest(http.MethodGet, "/v1/images/edits", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("prompt", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("image", "image.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte("not reached without a prompt"))
		_ = writer.Close()
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("image", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("prompt", "make it blue")
		_ = writer.Close()
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("request total size", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(""))
		r.Header.Set("Content-Type", "multipart/form-data; boundary=image-edit-test")
		r.ContentLength = maxImageEditRequestBytes + 1
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, r)
		assertInvalidImageEditResponse(t, w, http.StatusRequestEntityTooLarge)
	})
}

func TestBuildImageEditAttachmentsFromRepeatedFields(t *testing.T) {
	files := []imageEditTestFile{
		{field: "image", name: "first.png", data: imageEditTestBytes("image/png")},
		{field: "image", name: "second.jpg", data: imageEditTestBytes("image/jpeg")},
		{field: "image", name: "third.webp", data: imageEditTestBytes("image/webp")},
	}
	attachments, validationErr := buildImageEditAttachments(imageEditFileHeaders(parseImageEditTestForm(t, files)))
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if len(attachments) != len(files) {
		t.Fatalf("attachments=%d want %d", len(attachments), len(files))
	}
	for i, attachment := range attachments {
		if attachment.Type != "image" || attachment.Name != files[i].name {
			t.Fatalf("attachment[%d]=%+v", i, attachment)
		}
		prefix := "data:" + attachment.MimeType + ";base64,"
		if !strings.HasPrefix(attachment.URL, prefix) {
			t.Fatalf("attachment[%d] URL missing prefix %q", i, prefix)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(attachment.URL, prefix))
		if err != nil {
			t.Fatalf("decode attachment[%d]: %v", i, err)
		}
		if !bytes.Equal(decoded, files[i].data) {
			t.Fatalf("attachment[%d] data mismatch", i)
		}
	}
}

func TestBuildImageEditAttachmentsBoundaries(t *testing.T) {
	t.Run("single image array compatibility", func(t *testing.T) {
		files := []imageEditTestFile{{field: "image[]", name: "legacy.png", data: imageEditTestBytes("image/png")}}
		attachments, validationErr := buildImageEditAttachments(imageEditFileHeaders(parseImageEditTestForm(t, files)))
		if validationErr != nil {
			t.Fatal(validationErr)
		}
		if len(attachments) != 1 || attachments[0].Name != "legacy.png" {
			t.Fatalf("attachments=%+v", attachments)
		}
	})

	t.Run("ten images", func(t *testing.T) {
		files := make([]imageEditTestFile, maxImageEditAttachments)
		for i := range files {
			files[i] = imageEditTestFile{field: "image", name: "image.png", data: imageEditTestBytes("image/png")}
		}
		attachments, validationErr := buildImageEditAttachments(imageEditFileHeaders(parseImageEditTestForm(t, files)))
		if validationErr != nil {
			t.Fatal(validationErr)
		}
		if len(attachments) != maxImageEditAttachments {
			t.Fatalf("attachments=%d want %d", len(attachments), maxImageEditAttachments)
		}
	})

	t.Run("eleven images", func(t *testing.T) {
		files := make([]imageEditTestFile, maxImageEditAttachments+1)
		for i := range files {
			files[i] = imageEditTestFile{field: "image", name: "image.png", data: imageEditTestBytes("image/png")}
		}
		attachments, validationErr := buildImageEditAttachments(imageEditFileHeaders(parseImageEditTestForm(t, files)))
		if validationErr == nil || validationErr.status != http.StatusBadRequest {
			t.Fatalf("validation error=%v", validationErr)
		}
		if attachments != nil {
			t.Fatalf("attachments must not be truncated: got %d", len(attachments))
		}
	})

	t.Run("per image size", func(t *testing.T) {
		files := []imageEditTestFile{{field: "image", name: "image.png", data: imageEditTestBytes("image/png")}}
		headers := imageEditFileHeaders(parseImageEditTestForm(t, files))
		headers[0].Size = maxGeneratedImageBytes
		if _, validationErr := buildImageEditAttachments(headers); validationErr != nil {
			t.Fatalf("20 MiB boundary rejected: %v", validationErr)
		}
		headers[0].Size = maxGeneratedImageBytes + 1
		if _, validationErr := buildImageEditAttachments(headers); validationErr == nil || validationErr.status != http.StatusRequestEntityTooLarge {
			t.Fatalf("validation error=%v", validationErr)
		}
	})

	t.Run("unsupported image", func(t *testing.T) {
		files := []imageEditTestFile{{field: "image", name: "image.gif", data: imageEditTestBytes("invalid")}}
		attachments, validationErr := buildImageEditAttachments(imageEditFileHeaders(parseImageEditTestForm(t, files)))
		if validationErr == nil || validationErr.status != http.StatusBadRequest {
			t.Fatalf("validation error=%v", validationErr)
		}
		if attachments != nil {
			t.Fatalf("attachments must be empty: got %d", len(attachments))
		}
	})
}
