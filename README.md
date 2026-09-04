# prefix_updater

Keeps an AWS **managed prefix list** in sync with the current IP addresses of the
AWS endpoints an ECS Fargate task needs in order to start:

| Service          | Hostname                                  |
| ---------------- | ----------------------------------------- |
| ECR API          | `api.ecr.<region>.amazonaws.com`          |
| ECR Docker       | `dkr.ecr.<region>.amazonaws.com` and `<account>.dkr.ecr.<region>.amazonaws.com` |
| Secrets Manager  | `secretsmanager.<region>.amazonaws.com`   |
| CloudWatch Logs  | `logs.<region>.amazonaws.com`             |

Every 30 seconds it resolves those hostnames, and if the address set changed
since the last check (or this is the first check) it updates the prefix list.
Errors are logged and the loop continues, so a transient DNS or EC2 problem
never stops the process.

Use it to attach the resulting prefix list to a security group egress rule, so a
Fargate task in a private subnet can pull images and write logs without an
allow-all `0.0.0.0/0` rule.

## Install

```sh
go install github.com/tapitalee/prefix_updater@latest
```

Or download a `linux/amd64` or `linux/arm64` binary from the
[releases](https://github.com/tapitalee/prefix_updater/releases). Release
binaries are built with `CGO_ENABLED=0`, so they are static and run on any
distribution, including scratch/distroless containers.

## Usage

```sh
prefix_updater pl-0123456789abcdef0
```

or

```sh
PREFIX_LIST_ID=pl-0123456789abcdef0 prefix_updater
```

Every flag has an environment variable equivalent:

| Flag                     | Env var                | Default                                  | Meaning |
| ------------------------ | ---------------------- | ---------------------------------------- | ------- |
| `--prefix-list-id`       | `PREFIX_LIST_ID`       | –                                        | Prefix list to update; may also be the first positional argument |
| `--region`               | `REGION`               | resolved SDK region                      | Region used for the endpoint hostnames |
| `--interval`             | `INTERVAL`             | `30s`                                    | Delay between cycles (a bare number means seconds) |
| `--ip-ttl`               | `IP_TTL`               | `1h`                                     | How long an IP is kept after it was last seen in DNS; `0` keeps only the latest answer |
| `--dns-timeout`          | `DNS_TIMEOUT`          | `10s`                                    | Timeout for one resolution round |
| `--aws-timeout`          | `AWS_TIMEOUT`          | `2m`                                     | Timeout for one whole cycle |
| `--services`             | `SERVICES`             | `api.ecr,dkr.ecr,secretsmanager,logs`    | Comma separated service keys to resolve |
| `--extra-hosts`          | `EXTRA_HOSTS`          | –                                        | Extra hostnames to resolve verbatim |
| `--registry-host`        | `REGISTRY_HOST`        | `true`                                   | Also resolve `<account>.dkr.ecr.<region>` |
| `--description-prefix`   | `DESCRIPTION_PREFIX`   | `prefix_updater`                         | Marks the entries owned by this program |
| `--manage-all`           | `MANAGE_ALL`           | `false`                                  | Allow removing entries this program does not own |
| `--max-changes-per-call` | `MAX_CHANGES_PER_CALL` | `50`                                     | Entry changes per `ModifyManagedPrefixList` call |
| `--dry-run`              | `DRY_RUN`              | `false`                                  | Log the changes without applying them |
| `--once`                 | `ONCE`                 | `false`                                  | Run a single cycle and exit |
| `--log-level`            | `LOG_LEVEL`            | `info`                                   | `debug`, `info`, `warn` or `error` |
| `--log-format`           | `LOG_FORMAT`           | `text`                                   | `text` or `json` |

`--services` also accepts `ecr`, `s3`, `ecs`, `ecs-agent`, `ecs-telemetry`,
`ssm`, `ssmmessages`, `ec2messages`, `kms`, `sts`, `elasticloadbalancing`,
`monitoring` and `xray`. Adding `s3` is common, because ECR image layers are
served from S3.

Credentials and the region come from the standard AWS chain (environment,
shared config, ECS/EC2 task role or IMDS).

## Behaviour worth knowing

- **Ownership.** Only entries whose description starts with
  `--description-prefix` are ever removed, so manually added CIDRs in the same
  prefix list survive. `--manage-all` opts out of that protection.
- **Address pools.** AWS returns a rotating subset of a much larger address pool
  for these endpoints, so a single lookup is not the whole truth. Addresses are
  therefore accumulated and only removed once they have not been returned for
  `--ip-ttl`. Set `--ip-ttl 0` for strict "latest answer only" behaviour.
- **Partial failures.** If some lookups fail the cycle still adds new
  addresses but performs no removals, so a DNS blip cannot blackhole traffic. If
  every lookup fails the cycle fails and nothing is changed.
- **Safety rails.** The list is never emptied, `MaxEntries` overflow is reported
  instead of half-applied, and cycles are skipped while EC2 reports a
  modification in progress.
- **Resilience.** Panics are recovered, logged with a stack trace, and the loop
  continues on the next interval. `SIGINT`/`SIGTERM` shut it down cleanly.

## IAM permissions

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeManagedPrefixLists",
        "ec2:GetManagedPrefixListEntries"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "ec2:ModifyManagedPrefixList",
      "Resource": "arn:aws:ec2:REGION:ACCOUNT:prefix-list/pl-0123456789abcdef0"
    },
    {
      "Effect": "Allow",
      "Action": "sts:GetCallerIdentity",
      "Resource": "*"
    }
  ]
}
```

`sts:GetCallerIdentity` is only used to derive the account specific ECR registry
hostname; without it the program logs a warning and carries on.

Size the prefix list's `MaxEntries` generously: with the four default services
and a one hour TTL, expect a few dozen entries.

## Development

```sh
just build      # build ./prefix_updater
just test       # go test ./...
just check      # fmt, vet and test
just format     # goimports -w
just build-all  # CGO-free linux amd64 + arm64 binaries into dist/
```

## Releasing

Tags drive releases:

```sh
just tag    # tapit nextgitrelease, annotate, push
just retag  # move the latest release tag to HEAD and force push
```

Pushing a `v*` tag triggers `.github/workflows/release.yml`, which runs the
tests, builds `linux/amd64` and `linux/arm64` binaries without CGO, and
publishes them with SHA256 sums on the GitHub release.
