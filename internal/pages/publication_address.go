// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"errors"
	"math/big"
	"net/url"
	"strings"

	"gitea.dev/page/internal/gitea"
)

const ownerAlphabet = "-._0123456789abcdefghijklmnopqrstuvwxyz"
const ownerDirectAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz-"

func defaultSiteRoute(site gitea.Site, rootHost string) (host, base string, ok bool) {
	host = ownerHost(site.OwnerName, rootHost)
	if host == "" {
		return "", "", false
	}
	if strings.EqualFold(site.RepoName, ".pages") {
		return host, "/", true
	}
	return host, "/" + url.PathEscape(strings.ToLower(site.RepoName)) + "/", true
}

func ownerHost(owner, root string) string {
	label, err := encodeOwnerDNSLabel(strings.ToLower(owner))
	if err != nil || root == "" {
		return ""
	}
	return label + "." + normalizeHost(root)
}

func encodeOwnerDNSLabel(owner string) (string, error) {
	if owner == "" || len(owner) > 40 {
		return "", errors.New("invalid owner")
	}
	direct := !strings.HasPrefix(owner, "x-") && owner[0] != '-' && owner[len(owner)-1] != '-'
	for i := 0; i < len(owner); i++ {
		c := owner[i]
		if strings.IndexByte(ownerAlphabet, c) < 0 {
			return "", errors.New("invalid owner")
		}
		if strings.IndexByte(ownerDirectAlphabet, c) < 0 {
			direct = false
		}
	}
	if direct {
		return owner, nil
	}
	number := new(big.Int)
	forty := big.NewInt(40)
	for i := 0; i < len(owner); i++ {
		number.Mul(number, forty)
		number.Add(number, big.NewInt(int64(strings.IndexByte(ownerAlphabet, owner[i])+1)))
	}
	return "x-" + number.Text(36), nil
}
