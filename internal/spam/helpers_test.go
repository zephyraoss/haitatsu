package spam

import "net"

func parseIP(value string) net.IP { return net.ParseIP(value) }
