// Package awsx wraps the AWS calls prefix_updater needs.
package awsx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// AddressFamily of a managed prefix list.
type AddressFamily string

const (
	// FamilyIPv4 is an IPv4 prefix list.
	FamilyIPv4 AddressFamily = "IPv4"
	// FamilyIPv6 is an IPv6 prefix list.
	FamilyIPv6 AddressFamily = "IPv6"
)

// PrefixList is the subset of managed prefix list metadata we care about.
type PrefixList struct {
	ID            string
	Name          string
	Version       int64
	State         string
	AddressFamily AddressFamily
	MaxEntries    int32
}

// Stable reports whether the prefix list can be modified right now.
func (p PrefixList) Stable() bool {
	return !strings.HasSuffix(p.State, "-in-progress")
}

// Failed reports whether the prefix list is in a failed state.
func (p PrefixList) Failed() bool {
	return strings.HasSuffix(p.State, "-failed")
}

// Entry is a single prefix list entry.
type Entry struct {
	CIDR        string
	Description string
}

// EC2API is the EC2 surface used by Client, kept small so it can be faked.
type EC2API interface {
	DescribeManagedPrefixLists(ctx context.Context, in *ec2.DescribeManagedPrefixListsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeManagedPrefixListsOutput, error)
	GetManagedPrefixListEntries(ctx context.Context, in *ec2.GetManagedPrefixListEntriesInput, optFns ...func(*ec2.Options)) (*ec2.GetManagedPrefixListEntriesOutput, error)
	ModifyManagedPrefixList(ctx context.Context, in *ec2.ModifyManagedPrefixListInput, optFns ...func(*ec2.Options)) (*ec2.ModifyManagedPrefixListOutput, error)
}

// STSAPI is the STS surface used by Client.
type STSAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// Client talks to EC2 and STS.
type Client struct {
	EC2 EC2API
	STS STSAPI
}

// New builds a Client from a loaded AWS config.
func New(cfg aws.Config) *Client {
	return &Client{EC2: ec2.NewFromConfig(cfg), STS: sts.NewFromConfig(cfg)}
}

// ErrPrefixListNotFound is returned when the prefix list does not exist.
var ErrPrefixListNotFound = errors.New("prefix list not found")

// DescribePrefixList fetches the prefix list metadata.
func (c *Client) DescribePrefixList(ctx context.Context, id string) (PrefixList, error) {
	out, err := c.EC2.DescribeManagedPrefixLists(ctx, &ec2.DescribeManagedPrefixListsInput{
		PrefixListIds: []string{id},
	})
	if err != nil {
		return PrefixList{}, fmt.Errorf("describe managed prefix list %s: %w", id, err)
	}
	if len(out.PrefixLists) == 0 {
		return PrefixList{}, fmt.Errorf("%w: %s", ErrPrefixListNotFound, id)
	}

	pl := out.PrefixLists[0]
	res := PrefixList{
		ID:            aws.ToString(pl.PrefixListId),
		Name:          aws.ToString(pl.PrefixListName),
		Version:       aws.ToInt64(pl.Version),
		State:         string(pl.State),
		AddressFamily: AddressFamily(aws.ToString(pl.AddressFamily)),
		MaxEntries:    aws.ToInt32(pl.MaxEntries),
	}
	switch res.AddressFamily {
	case FamilyIPv4, FamilyIPv6:
	default:
		return res, fmt.Errorf("prefix list %s has unsupported address family %q", id, res.AddressFamily)
	}
	return res, nil
}

// Entries lists every entry of the prefix list.
func (c *Client) Entries(ctx context.Context, id string) ([]Entry, error) {
	var entries []Entry
	paginator := ec2.NewGetManagedPrefixListEntriesPaginator(c.EC2, &ec2.GetManagedPrefixListEntriesInput{
		PrefixListId: aws.String(id),
		MaxResults:   aws.Int32(100),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("get managed prefix list entries %s: %w", id, err)
		}
		for _, e := range page.Entries {
			entries = append(entries, Entry{
				CIDR:        aws.ToString(e.Cidr),
				Description: aws.ToString(e.Description),
			})
		}
	}
	return entries, nil
}

// Modify applies one batch of entry changes and returns the new version.
func (c *Client) Modify(ctx context.Context, id string, currentVersion int64, adds, removes []Entry) (int64, error) {
	in := &ec2.ModifyManagedPrefixListInput{
		PrefixListId:   aws.String(id),
		CurrentVersion: aws.Int64(currentVersion),
	}
	for _, e := range adds {
		in.AddEntries = append(in.AddEntries, types.AddPrefixListEntry{
			Cidr:        aws.String(e.CIDR),
			Description: aws.String(e.Description),
		})
	}
	for _, e := range removes {
		in.RemoveEntries = append(in.RemoveEntries, types.RemovePrefixListEntry{
			Cidr: aws.String(e.CIDR),
		})
	}

	out, err := c.EC2.ModifyManagedPrefixList(ctx, in)
	if err != nil {
		return currentVersion, fmt.Errorf("modify managed prefix list %s: %w", id, err)
	}
	if out.PrefixList == nil {
		return currentVersion, fmt.Errorf("modify managed prefix list %s: empty response", id)
	}
	return aws.ToInt64(out.PrefixList.Version), nil
}

// WaitStable polls until the prefix list leaves an -in-progress state.
func (c *Client) WaitStable(ctx context.Context, id string, poll, timeout time.Duration) (PrefixList, error) {
	deadline := time.Now().Add(timeout)
	for {
		pl, err := c.DescribePrefixList(ctx, id)
		if err != nil {
			return pl, err
		}
		if pl.Stable() {
			if pl.Failed() {
				return pl, fmt.Errorf("prefix list %s is in state %s", id, pl.State)
			}
			return pl, nil
		}
		if time.Now().After(deadline) {
			return pl, fmt.Errorf("timed out waiting for prefix list %s to become stable (state %s)", id, pl.State)
		}
		select {
		case <-ctx.Done():
			return pl, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// AccountID returns the account ID of the current credentials.
func (c *Client) AccountID(ctx context.Context) (string, error) {
	out, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("get caller identity: %w", err)
	}
	id := aws.ToString(out.Account)
	if id == "" {
		return "", errors.New("get caller identity: empty account")
	}
	return id, nil
}
