package sanctions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fixture in the format of the OFAC files: no header, "-0-" for no value,
// programs in brackets, addresses in the remarks, and the end-of-file mark.
const sdnCSV = `36,"AEROCARIBBEAN AIRLINES",-0- ,"CUBA",-0- ,-0- ,-0- ,-0- ,-0- ,-0- ,-0- ,-0-
306,"HOSSEINI, Ali","individual","SDGT] [IRGC",-0- ,-0- ,-0- ,-0- ,-0- ,-0- ,-0- ,"DOB 01 Jan 1970; Digital Currency Address - XBT 1BoatSLRHtKNngkdXEeobR76b53LETtpyT; Digital Currency Address - ETH 0x8576aCC5C05D6Ce88f4e49bf65BdF0C62F91353C; Digital Currency Address - TRX TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8;"
4721,"MERSK VESSEL","vessel","IRAN",-0- ,-0- ,-0- ,-0- ,-0- ,-0- ,-0- ,-0-
` + "\x1a\n"

const altCSV = `306,101,"aka","HOSEYNI, Aly",-0-
999,102,"aka","NOBODY",-0-
`

func fixture(t *testing.T) *List {
	t.Helper()
	entries, err := ParseOFAC([]byte(sdnCSV), []byte(altCSV))
	require.NoError(t, err)
	return NewList(ProviderOFAC, 1, time.Now(), entries)
}

func TestParseOFAC(t *testing.T) {
	entries, err := ParseOFAC([]byte(sdnCSV), []byte(altCSV))
	require.NoError(t, err)
	require.Len(t, entries, 3, "the end-of-file mark is not an entry")
	assert.Equal(t, Entry{ID: 36, Name: "AEROCARIBBEAN AIRLINES", Type: TypeEntity, Programs: []string{"CUBA"}}, entries[0])
	ali := entries[1]
	assert.Equal(t, TypeIndividual, ali.Type)
	assert.Equal(t, []string{"SDGT", "IRGC"}, ali.Programs)
	assert.Equal(t, []string{"HOSEYNI, Aly"}, ali.Aliases)
	assert.Equal(t, []Address{
		{Currency: "XBT", Value: "1BoatSLRHtKNngkdXEeobR76b53LETtpyT"},
		{Currency: "ETH", Value: "0x8576aCC5C05D6Ce88f4e49bf65BdF0C62F91353C"},
		{Currency: "TRX", Value: "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"},
	}, ali.Addresses)

	_, err = ParseOFAC([]byte("\x1a\n"), nil)
	assert.Error(t, err, "a list without entries is refused")
}

func TestNormalizeName(t *testing.T) {
	assert.Equal(t, "ali hosseini", NormalizeName("HOSSEINI, Ali"))
	assert.Equal(t, "ali hosseini", NormalizeName("  Ali   Hosséini "))
	assert.Equal(t, "0x8576acc5c05d6ce88f4e49bf65bdf0c62f91353c", NormalizeAddress("0x8576aCC5C05D6Ce88f4e49bf65BdF0C62F91353C"))
	assert.Equal(t, "1BoatSLRHtKNngkdXEeobR76b53LETtpyT", NormalizeAddress(" 1BoatSLRHtKNngkdXEeobR76b53LETtpyT "), "base58 keeps its case")
}

func TestMatchName(t *testing.T) {
	l := fixture(t)
	m := l.MatchName("Ali Hosseini", DefaultThreshold)
	require.Len(t, m, 1)
	assert.Equal(t, 306, m[0].Entry.ID)
	assert.InDelta(t, 1.0, m[0].Score, 1e-9)

	m = l.MatchName("Aly Hoseyni", DefaultThreshold)
	require.Len(t, m, 1, "an other name of the list")
	m = l.MatchName("Ali Hoseini", DefaultThreshold)
	require.Len(t, m, 1, "a spelling close enough")
	assert.Less(t, m[0].Score, 1.0)

	assert.Empty(t, l.MatchName("Reza Tehrani", DefaultThreshold))
	assert.Empty(t, l.MatchName("Hosseini", DefaultThreshold), "one word does not tell a person")
	assert.Empty(t, l.MatchName("Mersk Vessel", DefaultThreshold), "vessels are not people")
	assert.Empty(t, l.MatchName("Ali Hoseini", 0.999), "below a strict threshold")
}

func TestMatchAddress(t *testing.T) {
	l := fixture(t)
	assert.Len(t, l.MatchAddress("0x8576acc5c05d6ce88f4e49bf65bdf0c62f91353c"), 1, "hex in any case")
	assert.Len(t, l.MatchAddress("TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"), 1)
	assert.Empty(t, l.MatchAddress("tekxitehnzsmse2xqrbj4w32run966rdz8"), "base58 is exact")
	assert.Empty(t, l.MatchAddress("TQn9Y2khEsLJW1ChVWFMSMeRDow5KcbLSE"))
}

func TestJaroWinkler(t *testing.T) {
	assert.InDelta(t, 0.961, jaroWinkler("martha", "marhta"), 0.001)
	assert.InDelta(t, 0.840, jaroWinkler("dwayne", "duane"), 0.001)
	assert.Zero(t, jaroWinkler("", "a"))
	assert.InDelta(t, 1.0, jaroWinkler("same", "same"), 1e-9)
}

func TestFetchOFAC(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ofac/downloads/sdn.csv":
			_, _ = w.Write([]byte(sdnCSV))
		case "/ofac/downloads/alt.csv":
			_, _ = w.Write([]byte(altCSV))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	sdn, alt, err := FetchOFAC(context.Background(), server.Client(), server.URL+"/ofac/downloads/sdn.csv")
	require.NoError(t, err)
	assert.Equal(t, sdnCSV, string(sdn))
	assert.Equal(t, altCSV, string(alt))

	_, _, err = FetchOFAC(context.Background(), server.Client(), server.URL+"/ofac/downloads/list.csv")
	assert.Error(t, err, "not an sdn.csv")
	_, _, err = FetchOFAC(context.Background(), server.Client(), server.URL+"/elsewhere/sdn.csv")
	assert.ErrorContains(t, err, "status 404")
}
