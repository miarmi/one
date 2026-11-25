package main

import (
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"lab/platform/appkit"
)

var poolPathRE = regexp.MustCompile(`^[0-9a-f]{2}/[0-9a-f]{64}$`)

// poolFile serves one saved file of the pool to someone who is signed in.
// Every file is a download, never a page: it goes out as an attachment with
// no sniffing and a policy that allows nothing. Only a PNG, JPEG or GIF whose
// first bytes agree with its type keeps that type, so a picture in a message
// can be shown; anything else is plain bytes.
//
// The one exception is a click on a picture in a message (SPEC 5): the address
// ending in /view shows a verified picture in the browser itself, full size,
// with the same headers; a file that is not a verified picture is still a
// download there.
func (a *app) poolFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	f, ok := a.store.FileByID(r.Context(), id)
	if !ok || !poolPathRE.MatchString(f.Path) {
		http.NotFound(w, r)
		return
	}
	file, err := a.imp.pool.open(f.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	head := make([]byte, 512)
	n, _ := file.Read(head)
	if _, err := file.Seek(0, 0); err != nil {
		appkit.Error("reading a pool file", err)
		http.Error(w, "The file could not be read.", http.StatusInternalServerError)
		return
	}
	ctype := "application/octet-stream"
	if isPicture(f) && http.DetectContentType(head[:n]) == strings.ToLower(f.ContentType) {
		ctype = strings.ToLower(f.ContentType)
	}
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, f.Filename)
	if name == "" {
		name = "file"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	kind := "attachment"
	if strings.HasSuffix(r.URL.Path, "/view") && ctype != "application/octet-stream" {
		kind = "inline"
	}
	disposition := mime.FormatMediaType(kind, map[string]string{"filename": name})
	if disposition == "" {
		disposition = kind
	}
	h.Set("Content-Disposition", disposition)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "", st.ModTime(), file)
}
