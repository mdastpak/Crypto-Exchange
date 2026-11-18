package sanctions

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// The OFAC list (#73). OFAC publishes the SDN list as CSV files without a
// header: sdn.csv, one row per entry, and alt.csv, its other names. The
// addresses on chain are in the remarks of an entry, as "Digital Currency
// Address - XBT 1A1zP1...;". A field that has no value is "-0-".

// ProviderOFAC names the OFAC list.
const ProviderOFAC = "ofac"

// maxDownload is the largest file of the list read: the SDN list is some
// megabytes.
const maxDownload = 64 << 20

// The columns of the files.
const (
	sdnID       = 0
	sdnName     = 1
	sdnType     = 2
	sdnPrograms = 3
	sdnRemarks  = 11
	sdnColumns  = 12

	altID      = 0
	altName    = 3
	altColumns = 4
)

var digitalAddress = regexp.MustCompile(`Digital Currency Address - ([A-Z0-9]+) ([A-Za-z0-9]+)`)

// value is a field without the marker of no value.
func value(field string) string {
	f := strings.TrimSpace(field)
	if f == "-0-" {
		return ""
	}
	return f
}

// ParseOFAC parses the SDN list and its other names. A row that is not an
// entry, such as the end-of-file mark that the files carry, is skipped; a
// list without entries is an error.
func ParseOFAC(sdn, alt []byte) ([]Entry, error) {
	rows, err := readCSV(sdn)
	if err != nil {
		return nil, fmt.Errorf("sanctions: sdn.csv: %w", err)
	}
	var entries []Entry
	index := map[int]int{}
	for _, row := range rows {
		if len(row) < sdnColumns {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(row[sdnID]))
		if err != nil || value(row[sdnName]) == "" {
			continue
		}
		e := Entry{ID: id, Name: value(row[sdnName]), Type: strings.ToLower(value(row[sdnType]))}
		if e.Type == "" {
			e.Type = TypeEntity
		}
		for _, p := range strings.Split(strings.Trim(value(row[sdnPrograms]), "[]"), "] [") {
			if p = strings.TrimSpace(p); p != "" {
				e.Programs = append(e.Programs, p)
			}
		}
		for _, m := range digitalAddress.FindAllStringSubmatch(row[sdnRemarks], -1) {
			e.Addresses = append(e.Addresses, Address{Currency: m[1], Value: m[2]})
		}
		index[id] = len(entries)
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, errors.New("sanctions: sdn.csv has no entries")
	}
	rows, err = readCSV(alt)
	if err != nil {
		return nil, fmt.Errorf("sanctions: alt.csv: %w", err)
	}
	for _, row := range rows {
		if len(row) < altColumns {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(row[altID]))
		i, ok := index[id]
		if err != nil || !ok || value(row[altName]) == "" {
			continue
		}
		entries[i].Aliases = append(entries[i].Aliases, value(row[altName]))
	}
	return entries, nil
}

func readCSV(raw []byte) ([][]string, error) {
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord, r.LazyQuotes = -1, true
	var rows [][]string
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
}

// FetchOFAC downloads sdn.csv from sdnURL, and alt.csv beside it.
func FetchOFAC(ctx context.Context, client *http.Client, sdnURL string) (sdn, alt []byte, err error) {
	u, err := url.Parse(sdnURL)
	if err != nil || path.Base(u.Path) != "sdn.csv" {
		return nil, nil, fmt.Errorf("sanctions: %q is not the URL of an sdn.csv", sdnURL)
	}
	if sdn, err = download(ctx, client, u.String()); err != nil {
		return nil, nil, err
	}
	u.Path = path.Join(path.Dir(u.Path), "alt.csv")
	if alt, err = download(ctx, client, u.String()); err != nil {
		return nil, nil, err
	}
	return sdn, alt, nil
}

func download(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sanctions: download %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sanctions: download %s: status %d", target, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("sanctions: download %s: %w", target, err)
	}
	if len(body) > maxDownload {
		return nil, fmt.Errorf("sanctions: download %s: larger than %d bytes", target, maxDownload)
	}
	return body, nil
}
