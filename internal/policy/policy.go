package policy

import (
    "os"
    "strconv"
    "strings"
)

type Mode string

const (
    Observe Mode = "observe"
    Suggest Mode = "suggest"
    Fix     Mode = "fix"
)

type Policy struct {
    Mode              Mode
    HPACoexistence    bool
    CPUThreshold      float64
    ScaleWindow       string
    MaxScaleStep      int
    MaxActionsPer10m  int
    NamespaceAllow    map[string]struct__
    ExcludedAnnotation string
    LLMEnabled        bool
    LogLevel          string
}

func LoadFromEnv() *Policy {
    parseBool := func(k string, d bool) bool { v:=os.Getenv(k); if v=="" { return d }; b, _ := strconv.ParseBool(v); return b }
    parseFloat := func(k string, d float64) float64 { v:=os.Getenv(k); if v=="" { return d }; f, _ := strconv.ParseFloat(v,64); return f }
    parseInt := func(k string, d int) int { v:=os.Getenv(k); if v=="" { return d }; i, _ := strconv.Atoi(v); return i }

    ns := map[string]struct{}{}
    for _, n := range strings.Split(os.Getenv("NAMESPACE_ALLOWLIST"), ",") {
        n = strings.TrimSpace(n)
        if n != "" { ns[n] = struct{}{} }
    }

    m := Mode(os.Getenv("AUTO_MODE"))
    if m=="" { m=Fix }

    return &Policy{
        Mode: m,
        HPACoexistence: parseBool("HPA_COEXISTENCE", true),
        CPUThreshold: parseFloat("SCALE_CPU_THRESHOLD", 0.8),
        ScaleWindow: os.Getenv("SCALE_WINDOW"),
        MaxScaleStep: parseInt("MAX_SCALE_STEP", 2),
        MaxActionsPer10m: parseInt("MAX_ACTIONS_PER_10M", 10),
        NamespaceAllow: ns,
        ExcludedAnnotation: os.Getenv("EXCLUDED_ANNOTATION"),
        LLMEnabled: parseBool("LLM_ENABLED", true),
        LogLevel: os.Getenv("LOG_LEVEL"),
    }
}
