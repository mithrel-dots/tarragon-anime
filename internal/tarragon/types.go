package tarragon

type Message struct {
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"`
	QueryID  string `json:"query_id,omitempty"`
	Text     string `json:"text,omitempty"`
	ResultID string `json:"result_id,omitempty"`
	Action   string `json:"action,omitempty"`
	Plugin   string `json:"plugin,omitempty"`
}

type Action struct {
	Name  string `json:"name"`
	Type  string `json:"type,omitempty"`
	Query string `json:"query,omitempty"`
}

type Result struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Score       float64  `json:"score,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Category    string   `json:"category,omitempty"`
	PreviewPath string   `json:"preview_path,omitempty"`
	Actions     []Action `json:"actions,omitempty"`
}

type Payload struct {
	Results []Result `json:"results"`
}

type response struct {
	Type    string  `json:"type"`
	QueryID string  `json:"query_id"`
	Data    Payload `json:"data"`
}

type selectResponse struct {
	Type    string `json:"type"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}
