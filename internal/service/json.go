package service

import "encoding/json"

func jsonUnmarshalStrings(raw []byte, out *[]string) error { return json.Unmarshal(raw, out) }
