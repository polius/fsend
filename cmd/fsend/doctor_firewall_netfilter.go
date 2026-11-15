package main

import (
	"strconv"
	"strings"
)

// Raw netfilter rules: `nft list ruleset`, and `iptables -S` for the
// tables nft doesn't show (the legacy backend) or where nft is missing.
// Both need root. When doctor does get to read them, it follows one
// connection through the rules: a new connection from a device on the
// LAN to a port of the listener's range, once as UDP and once as TCP.
//
// The walk is deliberately conservative. Each rule either certainly
// matches that connection, certainly doesn't, or might (an expression
// this file doesn't model, a source only some devices have, part of the
// port range). A verdict is reported only when the certain rules decide
// it and no "might" rule could change it; anything else is "unclear",
// and the row says the rules don't settle it. The few expressions that
// are modelled are the ones the stock rulesets of ufw, firewalld, Arch
// and Debian are made of; TestNetfilter holds the walk to what those
// rulesets did to real connections.

// tri is a three-valued match. The order matters: a rule's conditions
// are ANDed with min.
type tri int

const (
	triNo tri = iota
	triMaybe
	triYes
)

func (t tri) not() tri { return triYes - t }

func coverTri(c cover) tri {
	switch c {
	case coverAll:
		return triYes
	case coverSome:
		return triMaybe
	}
	return triNo
}

// nfRule is one rule, reduced to whether it matches the connection and
// what it then does: "accept", "drop" (reject too), "return", "jump" or
// "goto" with a target, "unknown" for a verdict this file can't read, or
// "" for a rule that only counts or logs.
type nfRule struct {
	match   tri
	verdict string
	target  string
}

// nfChain is a chain's rules, each still as its tokens: what a rule
// matches depends on the connection.
type nfChain struct {
	name   string
	base   bool   // hooked into the inbound path
	policy string // a base chain's policy: "accept" or "drop"
	rules  [][]string
}

// nfTable is one table, of nft or of iptables.
type nfTable struct {
	family, name string
	chains       map[string]*nfChain
	order        []string // chain names, as listed
	// read judges one of the table's rules against a connection: nft's
	// syntax or, for a table read with iptables -S, iptables'.
	read     func([]string, nfConn) nfRule
	iptables bool
	// unjudged marks a table of a family whose rules this file doesn't
	// follow (netdev, bridge): packets pass it before they reach the
	// tables that are followed, so if it can drop anything the answer is
	// unclear.
	unjudged bool
}

// nfConn is the connection the rules are judged against.
type nfConn struct {
	lan   lanNet
	proto string // "udp" or "tcp"
}

// Walk results.
const (
	nfNone   = iota // the chain ended (or returned) without a verdict
	nfAccept        // accepted
	nfDrop          // dropped or rejected
)

// nfWalk follows c through one chain.
type nfWalk struct {
	table     *nfTable
	conn      nfConn
	mayAccept bool // a "might" rule could accept
	mayDrop   bool // a "might" rule could drop
}

func (w *nfWalk) chain(name string, depth int) int {
	ch := w.table.chains[name]
	if ch == nil || depth > 32 {
		w.mayAccept, w.mayDrop = true, true
		return nfNone
	}
	for _, raw := range ch.rules {
		r := w.table.read(raw, w.conn)
		if r.match == triNo {
			continue
		}
		sure := r.match == triYes
		var res int
		switch r.verdict {
		case "accept":
			res = nfAccept
		case "drop":
			res = nfDrop
		case "return":
			if sure {
				return nfNone
			}
			// Might leave the chain here: what follows is uncertain.
			w.mayAccept, w.mayDrop = true, true
			continue
		case "jump", "goto":
			sub := &nfWalk{table: w.table, conn: w.conn}
			res = sub.chain(r.target, depth+1)
			if sure {
				w.mayAccept = w.mayAccept || sub.mayAccept
				w.mayDrop = w.mayDrop || sub.mayDrop
				if res != nfNone || r.verdict == "goto" {
					return res
				}
				continue
			}
			w.mayAccept = w.mayAccept || sub.mayAccept || res == nfAccept
			w.mayDrop = w.mayDrop || sub.mayDrop || res == nfDrop
			continue
		case "unknown":
			w.mayAccept, w.mayDrop = true, true
			continue
		default:
			continue
		}
		if sure {
			return res
		}
		if res == nfAccept {
			w.mayAccept = true
		} else {
			w.mayDrop = true
		}
	}
	return nfNone
}

// nfVerdict judges c against every base chain of the tables: the
// connection gets through only if none of them drops it. blockedBy names
// the first chain that drops it, or leaves it unclear.
func nfVerdict(tables []*nfTable, c nfConn) (a fwAnswer, blockedBy *nfChain, in *nfTable) {
	a = fwOpen
	for _, t := range tables {
		for _, name := range t.order {
			ch := t.chains[name]
			if !ch.base {
				continue
			}
			if t.unjudged {
				if a == fwOpen && ch.canDrop() {
					a, blockedBy, in = fwUnknown, ch, t
				}
				continue
			}
			w := &nfWalk{table: t, conn: c}
			res := w.chain(name, 0)
			if res == nfNone {
				res = nfAccept
				if ch.policy == "drop" {
					res = nfDrop
				}
			}
			switch {
			case res == nfDrop && !w.mayAccept:
				return fwBlocked, ch, t
			case res == nfDrop || w.mayDrop:
				if a == fwOpen {
					a, blockedBy, in = fwUnknown, ch, t
				}
			}
		}
	}
	return a, blockedBy, in
}

// canDrop reports whether a chain that isn't followed could stop a
// packet: it drops by default, or one of its rules drops, rejects or
// hands the packet on.
func (ch *nfChain) canDrop() bool {
	if ch.policy == "drop" {
		return true
	}
	for _, rule := range ch.rules {
		for _, tok := range rule {
			switch tok {
			case "drop", "reject", "jump", "goto", "queue", "vmap":
				return true
			}
		}
	}
	return false
}

// ---- nft list ruleset ------------------------------------------------

// nftTokens splits a line of nft output: quoted strings are one token
// (without the quotes), and braces and commas are tokens of their own.
func nftTokens(line string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	inQuote := false
	for _, r := range line {
		switch {
		case inQuote:
			if r == '"' {
				inQuote = false
				out = append(out, cur.String())
				cur.Reset()
			} else {
				cur.WriteRune(r)
			}
		case r == '"':
			flush()
			inQuote = true
		case r == ' ' || r == '\t':
			flush()
		case r == '{' || r == '}' || r == ',' || r == ';':
			flush()
			out = append(out, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseNft reads `nft list ruleset` into the tables a connection of the
// listener's address family passes. Base chains on the inbound path are
// those hooked at ingress, prerouting or input, of type filter or nat (a
// nat chain can drop, and can send the connection elsewhere).
func parseNft(ruleset string, v6 bool) []*nfTable {
	var tables []*nfTable
	var table *nfTable
	var chain *nfChain
	var pending []string // a rule whose braces continue on the next line
	skip := 0            // depth inside a block that isn't a chain (a set, a map)
	for _, line := range strings.Split(ruleset, "\n") {
		if i := strings.Index(line, "#"); i >= 0 && !strings.Contains(line[:i], `"`) {
			line = line[:i]
		}
		toks := nftTokens(line)
		if len(toks) == 0 {
			continue
		}
		opens, closes := 0, 0
		for _, t := range toks {
			switch t {
			case "{":
				opens++
			case "}":
				closes++
			}
		}
		switch {
		case skip > 0:
			skip += opens - closes
		case table == nil:
			if toks[0] == "table" && len(toks) >= 4 && toks[len(toks)-1] == "{" {
				table = &nfTable{family: toks[1], name: toks[2], chains: map[string]*nfChain{}, read: readNftRule,
					unjudged: toks[1] == "netdev" || toks[1] == "bridge"}
			}
		case chain == nil:
			switch {
			case toks[0] == "}":
				if table.unjudged || table.family == "inet" || (table.family == "ip" && !v6) || (table.family == "ip6" && v6) {
					tables = append(tables, table)
				}
				table = nil
			case toks[0] == "chain" && len(toks) >= 3 && toks[len(toks)-1] == "{":
				chain = &nfChain{name: toks[1]}
			default:
				skip = opens - closes // a set, a map, flags …
			}
		case len(pending) > 0:
			pending = append(pending, toks...)
			if balanced(pending) {
				chain.rules = append(chain.rules, pending)
				pending = nil
			}
		case toks[0] == "}":
			table.chains[chain.name] = chain
			table.order = append(table.order, chain.name)
			chain = nil
		case toks[0] == "type" && len(toks) >= 4 && toks[2] == "hook":
			// type filter hook input priority filter; policy drop;
			hook := toks[3]
			chain.base = (toks[1] == "filter" || toks[1] == "nat") && (hook == "input" || hook == "prerouting" || hook == "ingress")
			chain.policy = "accept"
			for i, t := range toks {
				if t == "policy" && i+1 < len(toks) {
					chain.policy = toks[i+1]
				}
			}
		case toks[0] == "policy" && len(toks) >= 2:
			chain.policy = toks[1]
		case toks[0] == "comment":
		case !balanced(toks):
			pending = toks
		default:
			chain.rules = append(chain.rules, toks)
		}
	}
	return tables
}

func balanced(toks []string) bool {
	n := 0
	for _, t := range toks {
		switch t {
		case "{":
			n++
		case "}":
			n--
		}
	}
	return n <= 0
}

// nftOperand reads what an expression is compared with: an optional
// "!=", then one value, a comma list or a { set }. vmap is true for a
// verdict map, whose elements are "key : verdict".
func nftOperand(toks []string, i int) (elems []string, neg, vmap bool, next int) {
	if i < len(toks) && (toks[i] == "!=" || toks[i] == "==") {
		neg = toks[i] == "!="
		i++
	}
	if i < len(toks) && toks[i] == "vmap" {
		vmap = true
		i++
	}
	if i >= len(toks) {
		return nil, neg, vmap, i
	}
	if toks[i] == "{" {
		i++
		var cur []string
		for i < len(toks) && toks[i] != "}" {
			if toks[i] == "," {
				elems = append(elems, strings.Join(cur, " "))
				cur = nil
			} else {
				cur = append(cur, toks[i])
			}
			i++
		}
		if len(cur) > 0 {
			elems = append(elems, strings.Join(cur, " "))
		}
		return elems, neg, vmap, i + 1
	}
	elems = append(elems, toks[i])
	i++
	for i+1 < len(toks) && toks[i] == "," {
		elems = append(elems, toks[i+1])
		i += 2
	}
	return elems, neg, vmap, i
}

func ifaceMatch(elems []string, iface string) tri {
	onlyLo := true
	for _, e := range elems {
		if e != "lo" {
			onlyLo = false
		}
		if iface == "" {
			continue
		}
		if p, wild := strings.CutSuffix(e, "*"); e == iface || (wild && strings.HasPrefix(iface, p)) {
			return triYes
		}
	}
	if iface == "" && !onlyLo {
		return triMaybe // the LAN interface is unknown, but it isn't loopback
	}
	return triNo
}

func protoMatch(elems []string, proto string) tri {
	num := map[string]string{"6": "tcp", "17": "udp"}
	for _, e := range elems {
		if e == proto || num[e] == proto {
			return triYes
		}
	}
	return triNo
}

func portsMatch(elems []string, rangeSep string) tri {
	c := coverNone
	for _, e := range elems {
		if strings.HasPrefix(e, "@") {
			return triMaybe // a named set, whose elements aren't read
		}
		lo, hi, ok := parsePorts(e, rangeSep)
		if !ok {
			continue // a service name: none is registered in the range
		}
		c = max(c, portCover(lo, hi))
	}
	return coverTri(c)
}

// nftVmap applies a verdict map, "key : verdict" elements, to r: the
// verdict of the key the connection certainly matches. No key matching
// means the rule doesn't; a key that only might match leaves the verdict
// unknown.
func nftVmap(r *nfRule, elems []string, keyMatch func(key string) tri) {
	verdict, unsure := "", false
	for _, e := range elems {
		k, v, ok := strings.Cut(e, " : ")
		if !ok {
			unsure = true
			continue
		}
		switch keyMatch(k) {
		case triYes:
			if verdict == "" {
				verdict = v
			}
		case triMaybe:
			unsure = true
		}
	}
	f := strings.Fields(verdict)
	switch {
	case unsure:
		r.verdict = "unknown"
	case len(f) == 0:
		r.match = triNo
	case len(f) == 2 && (f[0] == "jump" || f[0] == "goto"):
		r.verdict, r.target = f[0], f[1]
	case f[0] == "accept" || f[0] == "drop" || f[0] == "return":
		r.verdict = f[0]
	case f[0] == "continue":
	default:
		r.verdict = "unknown"
	}
}

func srcMatch(elems []string, lan lanNet) tri {
	c := coverNone
	for _, e := range elems {
		n := parseNet(e)
		if n == nil {
			return triMaybe // a range or a named set
		}
		c = max(c, lan.srcCover(n))
	}
	return coverTri(c)
}

func dstMatch(elems []string, lan lanNet) tri {
	for _, e := range elems {
		n := parseNet(e)
		if n == nil {
			return triMaybe
		}
		if lan.ip != nil && n.Contains(lan.ip) {
			return triYes
		}
	}
	return triNo
}

func has(elems []string, want string) bool {
	for _, e := range elems {
		if e == want {
			return true
		}
	}
	return false
}

// nftVerdictWord reports whether tok ends a rule's matches. Where an
// expression this file reads expects a field name ("ip saddr", "ct
// state") and finds one of these instead, the word before it was the
// value of an expression it doesn't model ("ether type ip accept"), and
// the verdict must not be swallowed with it.
func nftVerdictWord(tok string) bool {
	switch tok {
	case "accept", "drop", "reject", "return", "continue", "jump", "goto", "queue",
		"dnat", "snat", "redirect", "masquerade", "tproxy":
		return true
	}
	return false
}

func boolTri(b bool) tri {
	if b {
		return triYes
	}
	return triNo
}

// readNftRule judges one nft rule against c.
func readNftRule(toks []string, c nfConn) nfRule {
	r := nfRule{match: triYes}
	and := func(t tri, neg bool) {
		if neg {
			t = t.not()
		}
		r.match = min(r.match, t)
	}
	i := 0
	operand := func() ([]string, bool, bool) {
		elems, neg, vmap, next := nftOperand(toks, i)
		i = next
		return elems, neg, vmap
	}
	for i < len(toks) && r.match != triNo {
		t := toks[i]
		i++
		switch t {
		case "meta", "==", ";":
		case "iifname", "iif":
			elems, neg, vmap := operand()
			if vmap {
				nftVmap(&r, elems, func(k string) tri { return ifaceMatch([]string{k}, c.lan.iface) })
				return r
			}
			and(ifaceMatch(elems, c.lan.iface), neg)
		case "oifname", "oif":
			_, neg, _ := operand()
			and(triNo, neg) // nothing is leaving on the inbound path
		case "l4proto":
			elems, neg, vmap := operand()
			if vmap {
				nftVmap(&r, elems, func(k string) tri { return protoMatch([]string{k}, c.proto) })
				return r
			}
			and(protoMatch(elems, c.proto), neg)
		case "nfproto", "protocol":
			// meta nfproto ipv4, meta protocol ip ("ip protocol" is read
			// with the ip header below).
			family := func(k string) tri {
				return boolTri(((k == "ipv6" || k == "ip6") && c.lan.v6()) || ((k == "ipv4" || k == "ip") && !c.lan.v6()))
			}
			elems, neg, vmap := operand()
			if vmap {
				nftVmap(&r, elems, family)
				return r
			}
			m := triNo
			for _, e := range elems {
				m = max(m, family(e))
			}
			and(m, neg)
		case "pkttype":
			elems, neg, _ := operand()
			and(boolTri(has(elems, "host")), neg)
		case "ct":
			if i >= len(toks) || nftVerdictWord(toks[i]) {
				and(triMaybe, false)
				break
			}
			field := toks[i]
			i++
			elems, neg, vmap := operand()
			switch {
			case field == "state" && vmap:
				// ct state vmap { established : accept, invalid : drop }
				nftVmap(&r, elems, func(k string) tri { return boolTri(k == "new") })
				return r
			case vmap:
				r.verdict = "unknown"
				return r
			case field == "state":
				and(boolTri(has(elems, "new")), neg)
			case field == "status":
				and(triNo, neg) // dnat, snat …: not a plain new connection
			case field == "direction":
				and(boolTri(has(elems, "original")), neg)
			default:
				and(triMaybe, false)
			}
		case "ip", "ip6":
			if (t == "ip6") != c.lan.v6() {
				r.match = triNo
				return r
			}
			if i >= len(toks) || nftVerdictWord(toks[i]) {
				and(triMaybe, false)
				break
			}
			field := toks[i]
			i++
			elems, neg, vmap := operand()
			switch {
			case vmap:
				r.verdict = "unknown"
				return r
			case field == "saddr":
				and(srcMatch(elems, c.lan), neg)
			case field == "daddr":
				and(dstMatch(elems, c.lan), neg)
			case field == "protocol" || field == "nexthdr":
				and(protoMatch(elems, c.proto), neg)
			default:
				and(triMaybe, false)
			}
		case "tcp", "udp", "th":
			if t != "th" && t != c.proto {
				r.match = triNo
				return r
			}
			if i >= len(toks) || nftVerdictWord(toks[i]) {
				and(triMaybe, false)
				break
			}
			field := toks[i]
			i++
			if field != "dport" && field != "sport" {
				and(triMaybe, false) // flags and the like
				break
			}
			elems, neg, vmap := operand()
			switch {
			case vmap && field == "dport":
				nftVmap(&r, elems, func(k string) tri { return portsMatch([]string{k}, "-") })
				return r
			case vmap:
				r.verdict = "unknown"
				return r
			case field == "dport":
				and(portsMatch(elems, "-"), neg)
			default:
				and(triMaybe, false) // a receiver's source port is arbitrary
			}
		case "rt":
			// "rt type 0" is the IPv6 routing header, which a plain
			// connection doesn't carry; "rt mtu", "rt nexthop" and the like
			// are facts about the route.
			if i < len(toks) {
				switch toks[i] {
				case "mtu", "nexthop", "classid", "realm", "ipsec":
					and(triMaybe, false)
					continue
				}
			}
			r.match = triNo
			return r
		case "icmp", "icmpv6", "igmp", "esp", "ah", "sctp", "dccp", "udplite", "gre", "arp", "comp",
			"frag", "hbh", "dst", "mh", "exthdr":
			// Another protocol, or an IPv6 extension header a plain
			// connection doesn't carry.
			r.match = triNo
			return r
		case "fib":
			// fib daddr type local; fib saddr . iif oif missing (a
			// reverse-path filter, which a device on the link passes).
			subject := ""
			for i < len(toks) && toks[i] != "type" && toks[i] != "oif" && toks[i] != "oifname" {
				if subject == "" {
					subject = toks[i]
				}
				i++
			}
			if i >= len(toks) {
				and(triMaybe, false)
				break
			}
			kind := toks[i]
			i++
			elems, neg, _ := operand()
			switch {
			case subject == "daddr" && kind == "type":
				and(boolTri(has(elems, "local")), neg)
			case subject == "saddr" && kind != "type":
				and(boolTri(!has(elems, "missing")), neg)
			default:
				and(triMaybe, false)
			}
		case "limit":
			// limit rate [over] 3/minute [burst 10 packets]: a rule under
			// a rate applies to ordinary traffic; "over" only to a flood.
			if i < len(toks) && toks[i] == "rate" {
				i++
			}
			if i < len(toks) && toks[i] == "over" {
				i++
				and(triNo, false)
			}
			i++
			if i < len(toks) && toks[i] == "burst" {
				i += 3
			}
		case "counter":
			if i < len(toks) && toks[i] == "packets" {
				i += 4
			}
		case "log":
			for i+1 < len(toks) && (toks[i] == "prefix" || toks[i] == "level" || toks[i] == "group" || toks[i] == "snaplen" || toks[i] == "queue-threshold") {
				i += 2
			}
			if i+1 < len(toks) && toks[i] == "flags" {
				i += 2
			}
		case "comment":
			i++
		case "accept", "drop", "return":
			r.verdict = t
			return r
		case "continue":
			return r
		case "reject":
			r.verdict = "drop"
			return r
		case "jump", "goto":
			if i < len(toks) {
				r.verdict, r.target = t, toks[i]
			}
			return r
		case "xt":
			// An iptables extension nft has no words for, printed as
			// xt match "recent" or xt target "DNAT".
			kind, name := "", ""
			if i+1 < len(toks) {
				kind, name = toks[i], toks[i+1]
				i += 2
			}
			if kind != "target" {
				and(triMaybe, false)
				break
			}
			if !iptablesQuietTarget[name] {
				r.verdict = "unknown"
			}
			return r
		case "vmap", "queue", "dnat", "redirect", "tproxy":
			// A verdict this file can't follow, or the connection sent to
			// another address or port.
			r.verdict = "unknown"
			return r
		default:
			and(triMaybe, false)
		}
	}
	return r
}

// ---- iptables -S -----------------------------------------------------

// shellFields splits an iptables-save style line, keeping a quoted
// argument (a comment, a log prefix) whole.
func shellFields(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			have = true
		case (r == ' ' || r == '\t') && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// parseIptables reads `iptables -t <table> -S` into the table's chains.
// Its base chains on the inbound path are PREROUTING and INPUT. nil when
// the dump holds no chain.
func parseIptables(dump, table string) *nfTable {
	t := &nfTable{family: "ip", name: table, chains: map[string]*nfChain{}, read: readIptablesRule, iptables: true}
	chain := func(name string) *nfChain {
		ch := t.chains[name]
		if ch == nil {
			ch = &nfChain{name: name}
			t.chains[name] = ch
			t.order = append(t.order, name)
		}
		return ch
	}
	for _, line := range strings.Split(dump, "\n") {
		f := shellFields(line)
		switch {
		case len(f) >= 3 && f[0] == "-P":
			ch := chain(f[1])
			ch.policy = strings.ToLower(f[2])
			if ch.policy == "reject" {
				ch.policy = "drop"
			}
			ch.base = f[1] == "INPUT" || f[1] == "PREROUTING"
		case len(f) >= 2 && f[0] == "-N":
			chain(f[1])
		case len(f) >= 2 && f[0] == "-A":
			ch := chain(f[1])
			ch.rules = append(ch.rules, f[2:])
		}
	}
	if len(t.chains) == 0 {
		return nil
	}
	return t
}

// iptablesQuietTarget lists the iptables targets that let a packet go on
// to the next rule: they log, mark or count.
var iptablesQuietTarget = map[string]bool{
	"LOG": true, "NFLOG": true, "ULOG": true, "MARK": true, "CONNMARK": true, "TRACE": true, "NOTRACK": true, "CT": true,
	"AUDIT": true, "SET": true, "TOS": true, "DSCP": true, "TTL": true, "CLASSIFY": true, "TCPMSS": true, "CHECKSUM": true,
}

// readIptablesRule judges one `iptables -S` rule against c.
func readIptablesRule(args []string, c nfConn) nfRule {
	r := nfRule{match: triYes}
	neg := false
	and := func(t tri) {
		if neg {
			t = t.not()
		}
		neg = false
		r.match = min(r.match, t)
	}
	i := 0
	val := func() string {
		if i < len(args) {
			i++
			return args[i-1]
		}
		return ""
	}
	for i < len(args) && r.match != triNo {
		a := val()
		switch a {
		case "!":
			neg = true
		case "-m", "--match":
			// The module's own options follow as flags. These match
			// headers a plain connection doesn't carry.
			switch val() {
			case "rt", "frag", "hbh", "dst", "mh", "ah", "esp", "ipv6header", "icmp", "icmp6":
				r.match = triNo
			}
		case "-f", "--fragment":
			and(triNo)
		case "-i", "--in-interface":
			e := val()
			if p, wild := strings.CutSuffix(e, "+"); wild {
				e = p + "*"
			}
			and(ifaceMatch([]string{e}, c.lan.iface))
		case "-o", "--out-interface":
			val()
			and(triNo)
		case "-p", "--protocol":
			if p := val(); p != "all" {
				and(protoMatch([]string{p}, c.proto))
			} else {
				neg = false
			}
		case "-s", "--source":
			and(srcMatch([]string{val()}, c.lan))
		case "-d", "--destination":
			and(dstMatch([]string{val()}, c.lan))
		case "--dport", "--dports", "--destination-port", "--destination-ports":
			and(portsMatch(strings.Split(val(), ","), ":"))
		case "--sport", "--sports", "--source-port", "--source-ports", "--ports":
			val()
			neg = false
			r.match = min(r.match, triMaybe)
		case "--ctstate", "--state":
			and(boolTri(has(strings.Split(val(), ","), "NEW")))
		case "--dst-type":
			and(boolTri(has(strings.Split(val(), ","), "LOCAL")))
		case "--pkt-type":
			and(boolTri(val() == "unicast"))
		case "--icmp-type", "--icmpv6-type":
			val()
			r.match = triNo
		case "--syn":
			and(triYes) // a new TCP connection starts with a SYN
		case "--limit", "--limit-burst", "--comment":
			val()
			neg = false
		case "-j", "--jump", "-g", "--goto":
			switch target := val(); target {
			case "ACCEPT":
				r.verdict = "accept"
			case "DROP", "REJECT":
				r.verdict = "drop"
			case "RETURN":
				r.verdict = "return"
			case "QUEUE", "NFQUEUE", "DNAT", "REDIRECT", "TPROXY", "":
				r.verdict = "unknown"
			default:
				if iptablesQuietTarget[target] {
					break
				}
				r.verdict, r.target = "jump", target
				if a == "-g" || a == "--goto" {
					r.verdict = "goto"
				}
			}
			return r // what follows are the target's own options
		default:
			// An option this file doesn't model: the rule might match.
			if i < len(args) && !strings.HasPrefix(args[i], "-") && args[i] != "!" {
				val()
			}
			neg = false
			r.match = min(r.match, triMaybe)
		}
	}
	return r
}

// ---- the row ----------------------------------------------------------

// nfRules is what was read of the kernel's rules: `nft list ruleset`
// (noNft when there is no nft to run) and, by table name, the `iptables
// -t <table> -S` dumps of the tables nft doesn't show.
type nfRules struct {
	nft      string
	noNft    bool
	iptables map[string]string
}

// iptablesIsNft reads `iptables -V`: "iptables v1.8.10 (nf_tables)" for
// the variant that writes nft's tables, "(legacy)" — or, before 1.8, no
// word at all — for the one with tables of its own.
func iptablesIsNft(version string) bool {
	return strings.Contains(version, "nf_tables")
}

// legacyTablesToRead picks, from /proc/net/ip_tables_names (the legacy
// backend's loaded tables, one a line), those on the inbound path.
func legacyTablesToRead(names string) []string {
	var tables []string
	for _, want := range []string{"raw", "mangle", "filter", "security"} {
		for _, name := range strings.Fields(names) {
			if name == want {
				tables = append(tables, name)
			}
		}
	}
	return tables
}

// netfilterPorts judges the listener's ports from the rules read.
// filtered is false when no base chain is on the inbound path at all.
// fix opens the ports in the chain that stops them.
func netfilterPorts(rules nfRules, lan lanNet) (udp, tcp fwAnswer, filtered bool, fix []detailLine) {
	tables := parseNft(rules.nft, lan.v6())
	// In a fixed order: the first chain that stops the connection is the
	// one the fix names.
	for _, name := range []string{"raw", "mangle", "filter", "security", "nat"} {
		if t := parseIptables(rules.iptables[name], name); t != nil {
			tables = append(tables, t)
		}
	}
	for _, t := range tables {
		for _, ch := range t.chains {
			filtered = filtered || (ch.base && (len(ch.rules) > 0 || ch.policy == "drop"))
		}
	}
	udp, by, in := nfVerdict(tables, nfConn{lan, "udp"})
	var by2 *nfChain
	var in2 *nfTable
	tcp, by2, in2 = nfVerdict(tables, nfConn{lan, "tcp"})
	if by == nil {
		by, in = by2, in2
	}
	if by != nil && !in.unjudged {
		fix = netfilterFix(in, by, lan.v6())
	}
	return udp, tcp, filtered, fix
}

// netfilterFix inserts accept rules at the top of the chain that stops
// the listener's ports. Tables that iptables manages are changed with
// iptables, also when they were read through nft, which refuses to touch
// them cleanly. nil for an iptables table other than filter: there is no
// one-line fix to print.
func netfilterFix(t *nfTable, ch *nfChain, v6 bool) []detailLine {
	if t.name == "filter" && ch.name == "INPUT" && (t.iptables || t.family != "inet") {
		tool := "iptables"
		if v6 {
			tool = "ip6tables"
		}
		return noteRow("", "").with("to open them (until the next reboot; add them to your saved rules to keep them):").run(
			"sudo "+tool+" -I INPUT -p udp --dport 50000:50999 -j ACCEPT",
			"sudo "+tool+" -I INPUT -p tcp --dport 50000:50999 -j ACCEPT").detail
	}
	if t.iptables {
		return nil
	}
	at := strconv.Quote(t.name) + " " + strconv.Quote(ch.name)
	if !strings.ContainsAny(t.name+ch.name, " \t") {
		at = t.name + " " + ch.name
	}
	return noteRow("", "").with("to open them (until the next reboot; add the rules to your ruleset file to keep them):").run(
		"sudo nft insert rule "+t.family+" "+at+" udp dport 50000-50999 accept",
		"sudo nft insert rule "+t.family+" "+at+" tcp dport 50000-50999 accept").detail
}

// netfilterCheck is the row for rules read straight from the kernel.
// probe is what the inbound test saw.
func netfilterCheck(rules nfRules, lan lanNet, probe udpProbe) doctorCheck {
	name := "nftables"
	if rules.noNft {
		name = "iptables"
	}
	udp, tcp, filtered, fix := netfilterPorts(rules, lan)
	if !filtered && probe.result != probeDropped {
		return passRow("firewall", "no firewall rules filter incoming connections ("+name+" has none on the inbound path)")
	}
	return portsCheck(name, udp, tcp, probe, fix...)
}

// netfilterUnreadable is the row for a machine whose kernel rules can't
// be read, and which runs neither ufw nor firewalld to ask instead. why
// says what stops the reading, advice how to get past it ("" when there
// is no way).
func netfilterUnreadable(why, advice string, probe udpProbe) doctorCheck {
	const inactive = "ufw and firewalld aren't active here, and "
	detail := detailLine{text: advice + "fsend listens on UDP and TCP " + lanPorts + "."}
	switch probe.result {
	case probeDropped:
		return warnRow("firewall", probe.seen("a firewall on this machine")+"; "+inactive+why).add(detail)
	case probeThrough:
		return noteRow("firewall", "a test datagram to UDP port "+strconv.Itoa(probe.port)+
			" came in unhindered, so devices on this network should reach fsend over UDP; whether TCP is filtered can't be told: "+inactive+why).add(detail)
	}
	return noteRow("firewall", "can't tell: "+inactive+why).add(detail)
}
