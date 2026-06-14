package dashboards

import (
	"encoding/json"
	"sort"
)

// DashboardSemantics is a stable fingerprint for dashboard parity tests.
type DashboardSemantics struct {
	UID    string            `json:"uid"`
	Title  string            `json:"title"`
	Tags   []string          `json:"tags"`
	Panels []PanelSemantics  `json:"panels"`
	Links  []LinkSemantics   `json:"links,omitempty"`
	Vars   []VariableSemantics `json:"variables,omitempty"`
}

// PanelSemantics captures panel type, layout, queries, and transformation chain.
type PanelSemantics struct {
	ID              uint32   `json:"id,omitempty"`
	Type            string   `json:"type"`
	Title           string   `json:"title"`
	GridPos         gridPos  `json:"gridPos,omitempty"`
	Exprs           []string `json:"exprs,omitempty"`
	TransformationIDs []string `json:"transformationIds,omitempty"`
}

type gridPos struct {
	H uint32 `json:"h"`
	W uint32 `json:"w"`
	X uint32 `json:"x"`
	Y uint32 `json:"y"`
}

// LinkSemantics captures cross-dashboard links.
type LinkSemantics struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// VariableSemantics captures templating variable names and query definitions.
type VariableSemantics struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Definition string `json:"definition,omitempty"`
	Multi      bool   `json:"multi,omitempty"`
	IncludeAll bool   `json:"includeAll,omitempty"`
}

type rawDashboard struct {
	UID    string       `json:"uid"`
	Title  string       `json:"title"`
	Tags   []string     `json:"tags"`
	Links  []rawLink    `json:"links"`
	Panels []rawPanel   `json:"panels"`
	Templating struct {
		List []rawVariable `json:"list"`
	} `json:"templating"`
}

type rawLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type rawPanel struct {
	ID              *uint32           `json:"id"`
	Type            string            `json:"type"`
	Title           string            `json:"title"`
	GridPos         *gridPos          `json:"gridPos"`
	Targets         []rawTarget       `json:"targets"`
	Transformations []rawTransformation `json:"transformations"`
}

type rawTarget struct {
	Expr string `json:"expr"`
}

type rawTransformation struct {
	ID string `json:"id"`
}

type rawVariable struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Definition  string `json:"definition"`
	Multi       bool   `json:"multi"`
	IncludeAll  bool   `json:"includeAll"`
}

// SemanticsFromJSON extracts a comparable dashboard fingerprint from marshaled JSON.
func SemanticsFromJSON(data []byte) (DashboardSemantics, error) {
	var raw rawDashboard
	if err := json.Unmarshal(data, &raw); err != nil {
		return DashboardSemantics{}, err
	}
	return semanticsFromRaw(raw), nil
}

// SemanticsFromDashboard marshals a built dashboard and extracts semantics.
func SemanticsFromDashboard(dash any) (DashboardSemantics, error) {
	data, err := json.Marshal(dash)
	if err != nil {
		return DashboardSemantics{}, err
	}
	return SemanticsFromJSON(data)
}

func semanticsFromRaw(raw rawDashboard) DashboardSemantics {
	sem := DashboardSemantics{
		UID:   raw.UID,
		Title: raw.Title,
		Tags:  append([]string(nil), raw.Tags...),
	}
	sort.Strings(sem.Tags)

	for _, link := range raw.Links {
		sem.Links = append(sem.Links, LinkSemantics{Title: link.Title, URL: link.URL})
	}
	sort.Slice(sem.Links, func(i, j int) bool {
		return sem.Links[i].Title < sem.Links[j].Title
	})

	for _, v := range raw.Templating.List {
		if v.Name == "" {
			continue
		}
		sem.Vars = append(sem.Vars, VariableSemantics{
			Name:       v.Name,
			Type:       v.Type,
			Definition: v.Definition,
			Multi:      v.Multi,
			IncludeAll: v.IncludeAll,
		})
	}
	sort.Slice(sem.Vars, func(i, j int) bool {
		return sem.Vars[i].Name < sem.Vars[j].Name
	})

	for _, panel := range raw.Panels {
		ps := PanelSemantics{
			Type:  panel.Type,
			Title: panel.Title,
		}
		if panel.ID != nil {
			ps.ID = *panel.ID
		}
		if panel.GridPos != nil {
			ps.GridPos = *panel.GridPos
		}
		for _, target := range panel.Targets {
			if target.Expr != "" {
				ps.Exprs = append(ps.Exprs, target.Expr)
			}
		}
		sort.Strings(ps.Exprs)
		for _, tr := range panel.Transformations {
			ps.TransformationIDs = append(ps.TransformationIDs, tr.ID)
		}
		sem.Panels = append(sem.Panels, ps)
	}

	return sem
}
