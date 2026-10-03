package chain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"

	"github.com/livepeer/node/eth"
)

func formatResult(out io.Writer, mode string, value any) error {
	if mode == "json" {
		return json.NewEncoder(out).Encode(value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return err
	}
	return renderText(out, "", tree, "")
}
func renderText(out io.Writer, key string, value any, indent string) error {
	label := strings.ReplaceAll(key, "_", " ")
	if label != "" {
		label = strings.ToUpper(label[:1]) + label[1:]
	}
	switch v := value.(type) {
	case map[string]any:
		if key != "" {
			if _, err := fmt.Fprintf(out, "%s%s:\n", indent, label); err != nil {
				return err
			}
			indent += "  "
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := renderText(out, k, v[k], indent); err != nil {
				return err
			}
		}
		return nil
	case []any:
		if _, err := fmt.Fprintf(out, "%s%s (%d):\n", indent, label, len(v)); err != nil {
			return err
		}
		for i, item := range v {
			if err := renderText(out, fmt.Sprintf("%d", i+1), item, indent+"  "); err != nil {
				return err
			}
		}
		return nil
	default:
		text := fmt.Sprint(value)
		if strings.HasSuffix(key, "_percent") {
			text += "%"
		}
		if value == nil {
			text = "none"
		}
		if raw, ok := value.(string); ok {
			if units, ok := new(big.Int).SetString(raw, 10); ok {
				switch {
				case key == "inflation" || key == "inflation_change" || key == "target_bonding_rate":
					text = eth.DecimalUnits(units, 7) + "%"
				case key == "round_lock_amount":
					text = eth.DecimalUnits(units, 4) + "%"
				case strings.HasSuffix(key, "_base_units"):
					label = strings.TrimSuffix(label, " base units")
					text = eth.DecimalUnits(units, 18) + " LPT"
				case strings.HasSuffix(key, "_wei") && !strings.Contains(key, "per_gas") && !strings.Contains(key, "cap") && !strings.Contains(key, "ceiling"):
					label = strings.TrimSuffix(label, " wei")
					text = eth.DecimalUnits(units, 18) + " ETH"
				}
			}
		}
		_, err := fmt.Fprintf(out, "%s%s: %s\n", indent, label, text)
		return err
	}
}
