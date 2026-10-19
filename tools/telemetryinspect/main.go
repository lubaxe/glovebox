package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lubaxe/glovebox/internal/telemetry"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "Usage: telemetryinspect /path/to/recording.mp4")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	result, err := telemetry.Extract(context.Background(), f, info.Size())
	if err != nil {
		panic(err)
	}
	speeds := []float64{}
	gears := map[uint64]int{}
	for _, s := range result.Samples {
		speeds = append(speeds, s.Speed)
		gears[s.Gear]++
	}
	min, max := 0.0, 0.0
	if len(speeds) > 0 {
		min = speeds[0]
	}
	for _, v := range speeds {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	out := map[string]any{"samples": len(result.Samples), "duration": result.Duration, "min_speed_mps": min, "max_speed_mps": max, "gears": gears}
	if len(result.Samples) > 0 {
		out["first_time"] = result.Samples[0].Time
		out["last_time"] = result.Samples[len(result.Samples)-1].Time
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
