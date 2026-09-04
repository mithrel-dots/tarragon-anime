package allanime

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

const bundleProfileFixture = `function dec(e){return e=e-(0),tab()[e]}function tab(){const e=["AAAAAA","AAAAA=","BBBBBB","BBBBB=","CCCCCC","CCCCC=","DDDDDD","DDDDD=","148","sXKiyl","epoch","host","buildId","lane","group"];return tab=function(){return e},tab()}function x(e,t){return dec(t)}const sf=[x(0,0)+x(0,1),x(0,2)+x(0,3),x(0,4)+x(0,5),x(0,6)+x(0,7)],Vd={saltMul:117,saltAdd:191,fragMul:129,fragAdd:11,bootPrefix:x(0,9)+":",join:"~",parts:[x(0,10),x(0,11),x(0,12),x(0,13),x(0,14)]}`

func TestParseBundleProfiles(t *testing.T) {
	profiles := parseBundleProfiles(bundleProfileFixture, "https://mkissa.to")
	if len(profiles) != 1 {
		t.Fatalf("parseBundleProfiles() returned %d profiles", len(profiles))
	}
	profile := profiles[0]
	if profile.BuildID != "148" || profile.BootPrefix != "sXKiyl:" || profile.BootJoin != "~" {
		t.Fatalf("profile = %#v", profile)
	}
	if profile.MaskHex == "" || len(profile.BootParts) != 5 || profile.BootParts[2] != "buildId" {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestRefreshProfilesResolvesAppBundleFromOrigin(t *testing.T) {
	for _, entry := range []string{
		`_app/immutable/entry/app.test.js`,
		`/_app/immutable/entry/app.test.js`,
	} {
		t.Run(entry, func(t *testing.T) {
			requests := make(chan string, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL.Path
				switch r.URL.Path {
				case "/":
					_, _ = w.Write([]byte(`import("` + entry + `")`))
				case "/_app/immutable/entry/app.test.js":
					_, _ = w.Write([]byte(`const cryptoChunk="../chunks/crypto.js"`))
				case "/_app/immutable/chunks/crypto.js":
					_, _ = w.Write([]byte(`const aaReq=true;` + bundleProfileFixture))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			profiles, err := NewClientWithEndpoints(server.Client(), server.URL+"/api", server.URL, server.URL).refreshProfiles(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(profiles) != 1 || profiles[0].BuildID != "148" {
				t.Fatalf("refreshProfiles() = %#v", profiles)
			}
			got := []string{<-requests, <-requests, <-requests}
			want := []string{"/", "/_app/immutable/entry/app.test.js", "/_app/immutable/chunks/crypto.js"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("bundle requests = %#v, want %#v", got, want)
			}
		})
	}
}

func TestRuntimeProfilePatternFindsMinifiedConstants(t *testing.T) {
	js := `const ad=Qt(953,1066),NA=6048e5,PA=Number(86400000),lm=[a()+b(),c()+d(),e()+f(),g()+h()],Tf={v:1,saltMul:240,saltAdd:242,fragMul:238,fragAdd:176,bootPrefix:x(),join:":",parts:[a(),b(),c(),d(),e()]};`
	match := runtimeProfileRE.FindStringSubmatch(js)
	if len(match) != 6 {
		t.Fatalf("runtime profile match = %#v", match)
	}
	want := []string{"ad", "NA", "PA", "lm", "Tf"}
	if !reflect.DeepEqual(match[1:], want) {
		t.Fatalf("runtime profile names = %#v, want %#v", match[1:], want)
	}
}
