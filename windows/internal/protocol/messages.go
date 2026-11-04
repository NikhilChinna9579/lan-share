package protocol

// Message structs for PROTOCOL.md §5. Each has a Type field set to its wire name.

type Hello struct {
	Type     string `json:"type"` // "hello"
	V        int    `json:"v"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

type Error struct {
	Type    string `json:"type"` // "error"
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type PairRequest struct {
	Type string `json:"type"` // "pair_request"
	Code string `json:"code"`
	Name string `json:"name"`
}

type PairOK struct {
	Type string `json:"type"` // "pair_ok"
	Name string `json:"name"`
}

type PairFail struct {
	Type   string `json:"type"` // "pair_fail"
	Reason string `json:"reason"`
}

type Folder struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type FoldersResponse struct {
	Type    string   `json:"type"` // "folders_response"
	Default string   `json:"default"`
	Folders []Folder `json:"folders"`
}

type OfferFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Path (optional) is a subfolder for this file inside the offer's folder/path,
	// "/"-separated: used when a whole folder is sent ("Holiday/Day 1"). The receiver
	// creates it, cleaning every part the same way as file names.
	Path string `json:"path,omitempty"`
}

type FileOffer struct {
	Type       string      `json:"type"` // "file_offer"
	TransferID string      `json:"transfer_id"`
	Folder     string      `json:"folder"`
	Path       string      `json:"path,omitempty"` // subfolder inside Folder, "/"-separated
	Files      []OfferFile `json:"files"`
}

// ListRequest asks what is inside a shared folder (or a subfolder of it).
type ListRequest struct {
	Type   string `json:"type"` // "list_request"
	Folder string `json:"folder"`
	Path   string `json:"path,omitempty"`
}

type ListFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"` // Unix milliseconds
}

type ListResponse struct {
	Type   string     `json:"type"` // "list_response"
	Folder string     `json:"folder"`
	Path   string     `json:"path"`
	Dirs   []string   `json:"dirs"`
	Files  []ListFile `json:"files"`
}

type FileAccept struct {
	Type       string `json:"type"` // "file_accept"
	TransferID string `json:"transfer_id"`
	// Offsets (optional), one per file: bytes the receiver already has from an
	// interrupted try. The sender continues each file from there.
	Offsets []int64 `json:"offsets,omitempty"`
}

type FileReject struct {
	Type       string `json:"type"` // "file_reject"
	TransferID string `json:"transfer_id"`
	Reason     string `json:"reason"`
}

type FileEnd struct {
	Type   string `json:"type"` // "file_end"
	Index  int    `json:"index"`
	SHA256 string `json:"sha256"`
}

type TransferDone struct {
	Type       string   `json:"type"` // "transfer_done"
	TransferID string   `json:"transfer_id"`
	Saved      []string `json:"saved"`
}

// PairingQR is the JSON encoded in the pairing QR code (PROTOCOL.md §5, Pairing).
type PairingQR struct {
	V    int    `json:"v"`
	Host string `json:"host"`
	Port int    `json:"port"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Code string `json:"code"`
}

// FetchRequest asks a device to send files from one of its shared folders. The
// device answers like a sender: file_offer, then (after file_accept) the bytes.
type FetchRequest struct {
	Type       string   `json:"type"` // "fetch_request"
	TransferID string   `json:"transfer_id"`
	Folder     string   `json:"folder"`
	Path       string   `json:"path,omitempty"`
	Files      []string `json:"files"`
}

// MkdirRequest creates a subfolder inside a shared folder (from a save-location picker).
type MkdirRequest struct {
	Type   string `json:"type"` // "mkdir_request"
	Folder string `json:"folder"`
	Path   string `json:"path,omitempty"`
	Name   string `json:"name"`
}

type MkdirOK struct {
	Type string `json:"type"` // "mkdir_ok"
	Name string `json:"name"` // the name actually created (cleaned)
}

// SearchRequest finds files by name in all of a device's shared folders.
type SearchRequest struct {
	Type  string `json:"type"` // "search_request"
	Query string `json:"query"`
}

type SearchHit struct {
	Folder   string `json:"folder"` // shared folder key
	Path     string `json:"path"`   // subfolder, "/"-separated ("" = top)
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"` // Unix milliseconds
}

type SearchResponse struct {
	Type    string      `json:"type"` // "search_response"
	Results []SearchHit `json:"results"`
}
