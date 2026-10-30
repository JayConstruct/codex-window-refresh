package store

import "sort"

// Bounded aggregates keep a worst-case response below the bridge body limit.
// Totals still describe the complete retained selection; omitted dimensions can
// be selected explicitly using filters instead of being silently counted as zero.
func boundTrafficPage(p *TrafficPage) {
	const groups = 32
	trimMap := func(values map[string]int) map[string]int {
		if len(values) <= groups {
			return values
		}
		p.AggregationLimited = true
		type item struct {
			id string
			n  int
		}
		items := make([]item, 0, len(values))
		for id, n := range values {
			items = append(items, item{id, n})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].n == items[j].n {
				return items[i].id < items[j].id
			}
			return items[i].n > items[j].n
		})
		result := map[string]int{}
		for _, v := range items[:groups] {
			result[v.id] = v.n
		}
		return result
	}
	if len(p.Accounts) > groups {
		p.Accounts = p.Accounts[:groups]
		p.AggregationLimited = true
	}
	if len(p.Projects) > groups {
		p.Projects = p.Projects[:groups]
		p.AggregationLimited = true
	}
	for i := range p.Projects {
		p.Projects[i].Accounts = trimMap(p.Projects[i].Accounts)
	}
	for _, rows := range []*[]TrafficDimension{&p.Callers, &p.Sessions} {
		if len(*rows) > groups {
			*rows = (*rows)[:groups]
			p.AggregationLimited = true
		}
		for i := range *rows {
			(*rows)[i].Accounts = trimMap((*rows)[i].Accounts)
		}
	}
	if len(p.Facets.Projects) > 128 {
		p.Facets.Projects = p.Facets.Projects[:128]
		p.FacetsLimited = true
	}
	for _, values := range []*[]string{&p.Facets.Accounts, &p.Facets.Callers, &p.Facets.Models} {
		if len(*values) > 128 {
			*values = (*values)[:128]
			p.FacetsLimited = true
		}
	}
}
