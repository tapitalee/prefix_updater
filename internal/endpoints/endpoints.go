// Package endpoints derives AWS service endpoint hostnames for a region.
package endpoints

import (
	"fmt"
	"sort"
	"strings"
)

// Host is a resolvable AWS endpoint hostname tagged with the logical service
// key it belongs to. The service key is used to build prefix list entry
// descriptions so the origin of every CIDR stays visible in the console.
type Host struct {
	Service string
	Name    string
}

// templates maps a logical service key to a hostname template taking the
// region and the partition DNS suffix.
var templates = map[string]string{
	// Required to boot an ECS Fargate task from a private subnet.
	"api.ecr":        "api.ecr.%s.%s",
	"dkr.ecr":        "dkr.ecr.%s.%s",
	"secretsmanager": "secretsmanager.%s.%s",
	"logs":           "logs.%s.%s",

	// Commonly needed extras, opt-in via --services.
	"ecr":                  "ecr.%s.%s",
	"s3":                   "s3.%s.%s",
	"ecs":                  "ecs.%s.%s",
	"ecs-agent":            "ecs-agent.%s.%s",
	"ecs-telemetry":        "ecs-telemetry.%s.%s",
	"ssm":                  "ssm.%s.%s",
	"ssmmessages":          "ssmmessages.%s.%s",
	"ec2messages":          "ec2messages.%s.%s",
	"kms":                  "kms.%s.%s",
	"sts":                  "sts.%s.%s",
	"elasticloadbalancing": "elasticloadbalancing.%s.%s",
	"monitoring":           "monitoring.%s.%s",
	"xray":                 "xray.%s.%s",
}

// DefaultServices are the services resolved when none are configured.
var DefaultServices = []string{"api.ecr", "dkr.ecr", "secretsmanager", "logs"}

// KnownServices returns every supported service key, sorted.
func KnownServices() []string {
	keys := make([]string, 0, len(templates))
	for k := range templates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DNSSuffix returns the partition DNS suffix for a region.
func DNSSuffix(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "amazonaws.com.cn"
	case strings.HasPrefix(region, "us-iso-"):
		return "c2s.ic.gov"
	case strings.HasPrefix(region, "us-isob-"):
		return "sc2s.sgov.gov"
	case strings.HasPrefix(region, "eu-isoe-"):
		return "cloud.adc-e.uk"
	case strings.HasPrefix(region, "us-isof-"):
		return "csp.hci.ic.gov"
	default:
		return "amazonaws.com"
	}
}

// Hostname returns the endpoint hostname for a service key in a region.
func Hostname(service, region string) (string, error) {
	tmpl, ok := templates[service]
	if !ok {
		return "", fmt.Errorf("unknown service %q (known: %s)", service, strings.Join(KnownServices(), ", "))
	}
	return fmt.Sprintf(tmpl, region, DNSSuffix(region)), nil
}

// RegistryHostname returns the account specific ECR registry hostname, which
// is the name a Fargate task actually pulls images from.
func RegistryHostname(accountID, region string) string {
	return fmt.Sprintf("%s.dkr.ecr.%s.%s", accountID, region, DNSSuffix(region))
}

// Hosts builds the host list for the given service keys plus any extra
// hostnames supplied verbatim by the operator.
func Hosts(services, extra []string, region string) ([]Host, error) {
	if region == "" {
		return nil, fmt.Errorf("region is empty; set AWS_REGION or --region")
	}

	seen := make(map[string]struct{})
	hosts := make([]Host, 0, len(services)+len(extra))

	for _, svc := range services {
		svc = strings.TrimSpace(svc)
		if svc == "" {
			continue
		}
		name, err := Hostname(svc, region)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		hosts = append(hosts, Host{Service: svc, Name: name})
	}

	for _, name := range extra {
		name = strings.TrimSpace(strings.TrimSuffix(name, "."))
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		hosts = append(hosts, Host{Service: ServiceKeyFor(name, region), Name: name})
	}

	return hosts, nil
}

// ServiceKeyFor guesses a short description label for an arbitrary hostname.
func ServiceKeyFor(name, region string) string {
	suffix := "." + region + "." + DNSSuffix(region)
	if trimmed := strings.TrimSuffix(name, suffix); trimmed != name {
		// 123456789012.dkr.ecr -> dkr.ecr
		if parts := strings.SplitN(trimmed, ".", 2); len(parts) == 2 && isAccountID(parts[0]) {
			return parts[1]
		}
		return trimmed
	}
	return name
}

func isAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
