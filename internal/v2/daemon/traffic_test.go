package daemon

import (
	"encoding/json"
	"testing"

	"codex-window-refresh/internal/v2/store"
)

func TestTrafficAPIReadOnlyEmptyFiltersAndStaticAssets(t *testing.T) {
	service, db, _ := historyService(t)
	var before, after int
	if err := db.QueryRow("PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"users", "plugin", "analysis"} {
		response := request(service, "GET", "/api/v2/traffic?kind="+kind+"&limit=7&page=1", "", "")
		var result store.TrafficPage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.InstanceID != service.instance || result.Records == nil || result.Total != 0 || result.Coverage.BaselineComplete {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	for _, query := range []string{"kind=bad", "kind=users&limit=0", "kind=users&limit=101", "page=x", "from=not-a-date", "kind=users&kind=plugin", "unexpected=value", "id=a&request_id=b", "search=%ZZ"} {
		if response := request(service, "GET", "/api/v2/traffic?"+query, "", ""); response.Code != 400 {
			t.Fatal(query, response.Code, response.Body.String())
		}
	}
	if response := request(service, "GET", "/api/v2/traffic?kind=users&id=absent", "", ""); response.Code != 404 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := request(service, "POST", "/api/v2/traffic", "", ""); response.Code != 404 {
		t.Fatal(response.Code)
	}
	for _, asset := range []string{"records.js", "records.css", "presentation.js"} {
		response := request(service, "GET", "/ui/"+asset, "", "")
		if response.Code != 200 || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(asset, response.Code)
		}
	}
	if err := db.QueryRow("PRAGMA data_version").Scan(&after); err != nil || before != after {
		t.Fatal("read changed state or sent model", before, after, err)
	}
}
