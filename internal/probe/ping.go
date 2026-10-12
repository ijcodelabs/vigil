package probe

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	pingCount = 3
	pingGap   = 200 * time.Millisecond
)

// checkPing sends a few ICMP echo requests and reports whether any was
// answered, with the average round trip of those that were.
//
// It first asks for a "ping socket", which needs no privileges where the
// system allows it (Linux: net.ipv4.ping_group_range), and falls back to a
// raw socket, which needs root or CAP_NET_RAW. An agent that may open
// neither reports the probe as failed.
func checkPing(ctx context.Context, host string, add func(string, float64)) (bool, time.Duration) {
	fail := func() (bool, time.Duration) {
		add("probe_packet_loss_ratio", 1)
		return false, -1
	}
	addrs, err := (&net.Resolver{}).LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return fail()
	}
	ip := addrs[0].IP
	for _, a := range addrs { // IPv4 first: the address most hosts are known by
		if a.IP.To4() != nil {
			ip = a.IP
			break
		}
	}
	v4 := ip.To4() != nil
	conn, datagram, err := listenICMP(v4)
	if err != nil {
		return fail()
	}
	defer conn.Close()

	var dst net.Addr = &net.IPAddr{IP: ip}
	if datagram {
		dst = &net.UDPAddr{IP: ip}
	}
	var typ icmp.Type = ipv4.ICMPTypeEcho
	proto := 1
	if !v4 {
		typ, proto = ipv6.ICMPTypeEchoRequest, 58
	}
	id := os.Getpid() & 0xffff
	token := rand.Uint64() // tells our answers from those to other pingers on a raw socket
	deadline, _ := ctx.Deadline()

	sent := map[int]time.Time{}
	var total time.Duration
	got := 0
	buf := make([]byte, 1500)
	for seq := 1; seq <= pingCount && ctx.Err() == nil; seq++ {
		data := make([]byte, 8)
		for i := range data {
			data[i] = byte(token >> (8 * i))
		}
		msg, _ := (&icmp.Message{Type: typ, Body: &icmp.Echo{ID: id, Seq: seq, Data: data}}).Marshal(nil)
		sent[seq] = time.Now()
		if _, err := conn.WriteTo(msg, dst); err != nil {
			continue
		}
		// Wait for this request's answer until the next one is due; the
		// last one may take until the probe's own deadline.
		wait := time.Now().Add(pingGap)
		if seq == pingCount {
			wait = deadline
		}
		for {
			conn.SetReadDeadline(wait)
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break
			}
			m, err := icmp.ParseMessage(proto, buf[:n])
			if err != nil {
				continue
			}
			echo, ok := m.Body.(*icmp.Echo)
			if !ok || (m.Type != ipv4.ICMPTypeEchoReply && m.Type != ipv6.ICMPTypeEchoReply) || len(echo.Data) != 8 {
				continue
			}
			var back uint64
			for i, b := range echo.Data {
				back |= uint64(b) << (8 * i)
			}
			at, known := sent[echo.Seq]
			if back != token || !known {
				continue
			}
			delete(sent, echo.Seq)
			total += time.Since(at)
			got++
			if echo.Seq == seq {
				if seq < pingCount {
					time.Sleep(time.Until(wait))
				}
				break
			}
		}
	}
	add("probe_packet_loss_ratio", 1-float64(got)/pingCount)
	if got == 0 {
		return false, -1
	}
	return true, total / time.Duration(got)
}

func listenICMP(v4 bool) (conn *icmp.PacketConn, datagram bool, err error) {
	dgram, raw, addr := "udp4", "ip4:icmp", "0.0.0.0"
	if !v4 {
		dgram, raw, addr = "udp6", "ip6:ipv6-icmp", "::"
	}
	if conn, err = icmp.ListenPacket(dgram, addr); err == nil {
		return conn, true, nil
	}
	if conn, err2 := icmp.ListenPacket(raw, addr); err2 == nil {
		return conn, false, nil
	} else {
		return nil, false, errors.Join(err, err2)
	}
}
