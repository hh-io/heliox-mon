package collector

import "testing"

func TestIsVirtualIface(t *testing.T) {
	virtual := []string{
		"",            // 空名视为虚拟
		"lo",          // 回环
		"docker0",     // 容器网桥
		"br-abc123",   // 自定义网桥
		"veth1a2b",    // 容器 veth pair
		"tun0",        // VPN 隧道
		"wg0",         // WireGuard
		"cloudflared", // Cloudflare 隧道
		"tailscale0",  // Tailscale
	}
	for _, iface := range virtual {
		if !isVirtualIface(iface) {
			t.Errorf("isVirtualIface(%q) = false, 期望 true（应被排除）", iface)
		}
	}

	physical := []string{"eth0", "ens3", "enp1s0", "wlan0"}
	for _, iface := range physical {
		if isVirtualIface(iface) {
			t.Errorf("isVirtualIface(%q) = true, 期望 false（物理网卡应统计）", iface)
		}
	}
}
