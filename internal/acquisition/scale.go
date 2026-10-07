package acquisition

// RawMax is the full-scale raw value of a 16-bit analog input (Version 01).
const RawMax = 65535.0

// Scale converts a raw register value into an engineering value:
//
//	PV = pv_min + (raw / 65535) × (pv_max - pv_min)
func Scale(raw uint16, min, max float64) float64 {
	return min + (float64(raw)/RawMax)*(max-min)
}
