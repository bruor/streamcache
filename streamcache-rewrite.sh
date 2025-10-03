#!/bin/bash

find "/path/to/strmfiles" -type f -name "*.strm" | while IFS= read -r file; do
    # Save original timestamps
    atime=$(stat -c %X "$file")  # Access time
    mtime=$(stat -c %Y "$file")  # Modification time

    # Read original URL from file
    original_url=$(<"$file")

    # Encode URL in base64
    encoded_url=$(echo -n "$original_url" | base64 -w 0)

    # Construct new URL
    new_url="http://<yourserver>:8080/u/$encoded_url"

    # Write new URL to file
    echo "$new_url" > "$file"

    # Restore original timestamps
    touch -a -d @"$atime" "$file"
    touch -m -d @"$mtime" "$file"
done
