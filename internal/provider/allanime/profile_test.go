package allanime

import "testing"

func TestParseBundleProfiles(t *testing.T) {
	js := `function dec(e){return e=e-(0),tab()[e]}function tab(){const e=["AAAAAA","AAAAA=","BBBBBB","BBBBB=","CCCCCC","CCCCC=","DDDDDD","DDDDD=","148","sXKiyl","epoch","host","buildId","lane","group"];return tab=function(){return e},tab()}function x(e,t){return dec(t)}const sf=[x(0,0)+x(0,1),x(0,2)+x(0,3),x(0,4)+x(0,5),x(0,6)+x(0,7)],Vd={saltMul:117,saltAdd:191,fragMul:129,fragAdd:11,bootPrefix:x(0,9)+":",join:"~",parts:[x(0,10),x(0,11),x(0,12),x(0,13),x(0,14)]}`
	profiles := parseBundleProfiles(js, "https://mkissa.to")
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
