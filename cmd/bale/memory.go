package main

import (
	"fmt"
	"strconv"
	"strings"
)

type pluginMemoryFlag struct{ value *uint64 }

func (flag pluginMemoryFlag) String() string { return strconv.FormatUint(*flag.value, 10) }

func (flag pluginMemoryFlag) Set(input string) error {
	number, multiplier := input, uint64(1)
	for _, unit := range []struct {
		suffix     string
		multiplier uint64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(input, unit.suffix) {
			number, multiplier = strings.TrimSuffix(input, unit.suffix), unit.multiplier
			break
		}
	}
	if number == "" || strings.Trim(number, "0123456789") != "" {
		return fmt.Errorf("invalid plugin memory size %q: use bytes or an integer with KiB, MiB or GiB", input)
	}
	value, err := strconv.ParseUint(number, 10, 64)
	if err != nil || value > ^uint64(0)/multiplier {
		return fmt.Errorf("plugin memory size %q overflows uint64", input)
	}
	*flag.value = value * multiplier
	return nil
}
