package main

import "encoding/json"

// Go's decoder accepts null for scalar fields and loses presence information.
// Check those cases and required nested members before dispatch or reservation.
func validateJSONFields(body []byte) *appError {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return unprocessable("Expected a message object")
	}
	if _, ok := fields["from"]; !ok {
		return unprocessable("from is required")
	}
	for name, value := range fields {
		if string(value) == "null" {
			return unprocessable(name + " must not be null")
		}
		switch name {
		case "from":
			if err := requiredMembers(value, "email"); err != nil {
				return err
			}
		case "to", "cc", "bcc", "replyTo":
			var addresses []json.RawMessage
			_ = json.Unmarshal(value, &addresses)
			if name != "replyTo" && len(addresses) == 0 {
				return unprocessable(name + " must not be empty")
			}
			for _, address := range addresses {
				if err := requiredMembers(address, "email"); err != nil {
					return err
				}
			}
		case "headers", "attachments":
			var entries []json.RawMessage
			_ = json.Unmarshal(value, &entries)
			required := []string{"name", "value"}
			if name == "attachments" {
				required = []string{"filename", "contentType", "content"}
			}
			for _, entry := range entries {
				if err := requiredMembers(entry, required...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func requiredMembers(value json.RawMessage, required ...string) *appError {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(value, &members); err != nil || members == nil {
		return unprocessable("Expected an object")
	}
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return unprocessable(name + " is required")
		}
	}
	for name, member := range members {
		if string(member) == "null" {
			return unprocessable(name + " must not be null")
		}
	}
	return nil
}
