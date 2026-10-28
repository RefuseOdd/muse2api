package engine

const (
	MaxReferenceImages     = 4
	MaxReferenceImageBytes = 10 * 1024 * 1024
	MaxReferenceTotalBytes = 24 * 1024 * 1024

	MaxChatFiles     = 4
	MaxChatFileBytes = 100 * 1024 * 1024
)

type ChatFile struct {
	Name     string `json:"name"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime"`
}

// EssentialCookies are the only cookies that determine whether an account can
// be used. hatch_gw is optional and must never block an import.
var EssentialCookies = []string{"hatch_sess", "hatch_vml"}

type Attachment struct {
	TID      string  `json:"tid"`
	HasVideo bool    `json:"hasVideo"`
	HasImg   bool    `json:"hasImg"`
	Src      string  `json:"src"`
	VSrc     string  `json:"vSrc"`
	ISrc     string  `json:"iSrc"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
}

type GenResult struct {
	Path       string  `json:"path"`
	Filename   string  `json:"filename"`
	Size       int64   `json:"size"`
	Ext        string  `json:"ext"`
	MIME       string  `json:"mime"`
	Kind       string  `json:"kind"`
	Via        string  `json:"via"`
	Attachment string  `json:"attachment"`
	W          float64 `json:"w"`
	H          float64 `json:"h"`
}

type SessionRenewResult struct {
	OK            bool
	Status        string
	VMID          string
	VMState       string
	WakeOK        bool
	WakeRequested bool
	Cookies       map[string]string
	CookiesExp    map[string]int64
}
