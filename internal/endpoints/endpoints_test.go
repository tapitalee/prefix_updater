package endpoints

import "testing"

func TestDNSSuffix(t *testing.T) {
	cases := map[string]string{
		"us-west-2":      "amazonaws.com",
		"eu-central-1":   "amazonaws.com",
		"us-gov-west-1":  "amazonaws.com",
		"cn-north-1":     "amazonaws.com.cn",
		"us-iso-east-1":  "c2s.ic.gov",
		"us-isob-east-1": "sc2s.sgov.gov",
	}
	for region, want := range cases {
		if got := DNSSuffix(region); got != want {
			t.Errorf("DNSSuffix(%q) = %q, want %q", region, got, want)
		}
	}
}

func TestHostname(t *testing.T) {
	cases := []struct {
		service, region, want string
	}{
		{"api.ecr", "us-west-2", "api.ecr.us-west-2.amazonaws.com"},
		{"dkr.ecr", "us-west-2", "dkr.ecr.us-west-2.amazonaws.com"},
		{"secretsmanager", "eu-west-1", "secretsmanager.eu-west-1.amazonaws.com"},
		{"logs", "ap-southeast-2", "logs.ap-southeast-2.amazonaws.com"},
		{"logs", "cn-north-1", "logs.cn-north-1.amazonaws.com.cn"},
	}
	for _, c := range cases {
		got, err := Hostname(c.service, c.region)
		if err != nil {
			t.Fatalf("Hostname(%q, %q): %v", c.service, c.region, err)
		}
		if got != c.want {
			t.Errorf("Hostname(%q, %q) = %q, want %q", c.service, c.region, got, c.want)
		}
	}

	if _, err := Hostname("nope", "us-west-2"); err == nil {
		t.Error("expected an error for an unknown service")
	}
}

func TestRegistryHostname(t *testing.T) {
	want := "123456789012.dkr.ecr.us-west-2.amazonaws.com"
	if got := RegistryHostname("123456789012", "us-west-2"); got != want {
		t.Errorf("RegistryHostname = %q, want %q", got, want)
	}
}

func TestHosts(t *testing.T) {
	hosts, err := Hosts(DefaultServices, []string{"example.com", "api.ecr.us-west-2.amazonaws.com", " "}, "us-west-2")
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	// 4 defaults + example.com; the duplicate api.ecr host is dropped.
	if len(hosts) != 5 {
		t.Fatalf("got %d hosts, want 5: %+v", len(hosts), hosts)
	}
	if hosts[0].Service != "api.ecr" || hosts[0].Name != "api.ecr.us-west-2.amazonaws.com" {
		t.Errorf("unexpected first host: %+v", hosts[0])
	}
	if last := hosts[4]; last.Name != "example.com" {
		t.Errorf("unexpected last host: %+v", last)
	}

	if _, err := Hosts(DefaultServices, nil, ""); err == nil {
		t.Error("expected an error for an empty region")
	}
	if _, err := Hosts([]string{"bogus"}, nil, "us-west-2"); err == nil {
		t.Error("expected an error for an unknown service")
	}
}

func TestServiceKeyFor(t *testing.T) {
	cases := map[string]string{
		"123456789012.dkr.ecr.us-west-2.amazonaws.com": "dkr.ecr",
		"api.ecr.us-west-2.amazonaws.com":              "api.ecr",
		"example.com":                                  "example.com",
	}
	for host, want := range cases {
		if got := ServiceKeyFor(host, "us-west-2"); got != want {
			t.Errorf("ServiceKeyFor(%q) = %q, want %q", host, got, want)
		}
	}
}
