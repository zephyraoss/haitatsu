package spam

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dnsblTimeout = 3 * time.Second

func dnsblListed(ctx context.Context, remoteIP string, zones []string) (bool, string) {
	ip := net.ParseIP(remoteIP)
	if ip == nil || len(zones) == 0 {
		return false, ""
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return false, ""
	}
	reversed := reverseIP(ip)
	if reversed == "" {
		return false, ""
	}
	names := make([]string, len(zones))
	listed := make([]bool, len(zones))
	var wg sync.WaitGroup
	for i, zone := range zones {
		zone = strings.TrimSuffix(strings.TrimSpace(zone), ".")
		if zone == "" {
			continue
		}
		names[i] = zone
		wg.Add(1)
		go func(i int, zone string) {
			defer wg.Done()
			lookupCtx, cancel := context.WithTimeout(ctx, dnsblTimeout)
			defer cancel()
			addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, reversed+"."+zone)
			if err != nil || len(addrs) == 0 {
				return
			}
			listed[i] = strings.HasPrefix(addrs[0].IP.String(), "127.")
		}(i, zone)
	}
	wg.Wait()
	for i, hit := range listed {
		if hit {
			return true, names[i]
		}
	}
	return false, ""
}

func reverseIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return strings.Join([]string{strconv.Itoa(int(v4[3])), strconv.Itoa(int(v4[2])), strconv.Itoa(int(v4[1])), strconv.Itoa(int(v4[0]))}, ".")
	}
	v6 := ip.To16()
	if v6 == nil {
		return ""
	}
	const hexDigits = "0123456789abcdef"
	nibbles := make([]string, 0, 32)
	for i := len(v6) - 1; i >= 0; i-- {
		nibbles = append(nibbles, string(hexDigits[v6[i]&0x0f]), string(hexDigits[v6[i]>>4]))
	}
	return strings.Join(nibbles, ".")
}
