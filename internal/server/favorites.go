package server

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"nclite/internal/store"
)

const nsOC = "http://owncloud.org/ns"

// proppatch applies the properties we understand (oc:favorite, stored per user)
// and acknowledges the rest without storing them.
func (d *davReq) proppatch() {
	type prop struct {
		name   xml.Name
		value  string
		remove bool
	}
	dec := xml.NewDecoder(io.LimitReader(d.r.Body, 1<<20))
	var props []prop
	depth, propDepth, cur, op := 0, -1, -1, ""
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 2 && (t.Name.Local == "set" || t.Name.Local == "remove"):
				op = t.Name.Local
			case t.Name.Local == "prop" && propDepth < 0:
				propDepth = depth
			case propDepth >= 0 && depth == propDepth+1:
				props = append(props, prop{name: t.Name, remove: op == "remove"})
				cur = len(props) - 1
			}
		case xml.CharData:
			if cur >= 0 && depth == propDepth+1 {
				props[cur].value += string(t)
			}
		case xml.EndElement:
			if depth == propDepth+1 {
				cur = -1
			}
			if depth == propDepth {
				propDepth = -1
			}
			if depth == 2 {
				op = ""
			}
			depth--
		}
	}

	node, err := d.s.fs.NodeFor(d.t.Owner, d.t.Rel)
	if err != nil {
		d.fail(err)
		return
	}
	for _, p := range props {
		if p.name.Space == nsOC && p.name.Local == "favorite" {
			if err := d.s.st.SetFavorite(d.user, node.ID, !p.remove && strings.TrimSpace(p.value) == "1"); err != nil {
				d.fail(err)
				return
			}
		}
	}

	var b strings.Builder
	b.WriteString(xml.Header + `<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns"><d:response><d:href>`)
	b.WriteString(xesc(d.r.URL.EscapedPath()))
	b.WriteString(`</d:href><d:propstat><d:prop>`)
	for _, p := range props {
		if pre, ok := nsPrefix[p.name.Space]; ok {
			b.WriteString("<" + pre + ":" + p.name.Local + "/>")
		} else {
			b.WriteString(`<x:` + p.name.Local + ` xmlns:x="` + xesc(p.name.Space) + `"/>`)
		}
	}
	b.WriteString(`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	d.w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	d.w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(d.w, b.String())
}

var reFavOne = regexp.MustCompile(`favorite[^>]*>\s*1\s*<`)

// report answers the one REPORT the iOS client sends: "all my favorites"
// (oc:filter-files with an oc:favorite rule). Other reports get an empty list.
func (d *davReq) report() {
	s := d.s
	body, _ := io.ReadAll(io.LimitReader(d.r.Body, 1<<20))

	type item struct {
		rel string
		ro  bool
		n   *store.Node
	}
	var items []item
	if bytes.Contains(body, []byte("filter-files")) && reFavOne.Match(body) {
		nodes, _ := s.st.FavoriteNodes(d.user)
		for _, n := range nodes {
			rel, ro, ok := s.fs.VisiblePath(d.user, n)
			if !ok || (d.rel != "" && rel != d.rel && !strings.HasPrefix(rel, d.rel+"/")) {
				continue
			}
			fresh, err := s.fs.NodeFor(n.Owner, n.Path)
			if err != nil {
				continue // vanished from disk
			}
			items = append(items, item{rel, ro, fresh})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	}

	d.w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	d.w.WriteHeader(http.StatusMultiStatus)
	bw := bufio.NewWriterSize(d.w, 32*1024)
	defer bw.Flush()
	bw.WriteString(xml.Header)
	bw.WriteString(`<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`)
	for _, it := range items {
		e := entry{fav: true, href: s.href(d.prefix, it.rel, it.n.IsDir), n: it.n, etag: it.n.ETag, ro: it.ro}
		if it.n.IsDir {
			e.size = s.st.DirSize(it.n.Owner, it.n.Path)
		} else {
			e.size = it.n.Size
			e.hasPrv = s.pv.Supported(it.n.Path)
		}
		s.writeEntry(bw, e)
	}
	bw.WriteString(`</d:multistatus>`)
}
