package sweexplore

import (
	"crypto/sha256"
	"fmt"
)

// Pair returns the exact native and treatment arms for one provider after the
// configuration-wide equality checks have passed.
func (config Config) Pair(provider Provider) (ArmConfig, ArmConfig, error) {
	var native, treatment ArmConfig
	for _, arm := range config.Arms {
		if arm.Provider != provider {
			continue
		}
		if arm.Mode == ModeNative {
			native = arm
		} else if arm.Mode == ModeLCTK {
			treatment = arm
		}
	}
	if native.ID == "" || treatment.ID == "" {
		return ArmConfig{}, ArmConfig{}, fmt.Errorf("provider %q has no complete pair", provider)
	}
	return native, treatment, nil
}

// CounterbalancedPair deterministically alternates which arm runs first. The
// hash prevents dataset ordering from correlating with treatment order while
// remaining reproducible without mutable campaign state.
func CounterbalancedPair(instanceID string, provider Provider, native, treatment ArmConfig) [2]ArmConfig {
	digest := sha256.Sum256([]byte(instanceID + "\x00" + string(provider)))
	if digest[0]&1 == 0 {
		return [2]ArmConfig{native, treatment}
	}
	return [2]ArmConfig{treatment, native}
}
