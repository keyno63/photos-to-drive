on replaceText(findText, replacementText, sourceText)
	set oldDelimiters to AppleScript's text item delimiters
	set AppleScript's text item delimiters to findText
	set textParts to text items of sourceText
	set AppleScript's text item delimiters to replacementText
	set cleanedText to textParts as text
	set AppleScript's text item delimiters to oldDelimiters
	return cleanedText
end replaceText

on cleanField(fieldValue)
	if fieldValue is missing value then return ""
	set cleanedText to fieldValue as text
	set cleanedText to my replaceText(tab, " ", cleanedText)
	set cleanedText to my replaceText(return, " ", cleanedText)
	set cleanedText to my replaceText(linefeed, " ", cleanedText)
	return cleanedText
end cleanField

using terms from application "/System/Applications/Photos.app"
	tell application "/System/Applications/Photos.app"
		launch
		set itemIDs to id of every media item
		set itemFilenames to filename of every media item
		set itemTitles to name of every media item
	end tell
end using terms from

set outputText to ""
repeat with itemIndex from 1 to count of itemIDs
	set outputText to outputText & my cleanField(item itemIndex of itemIDs) & tab & my cleanField(item itemIndex of itemFilenames) & tab & my cleanField(item itemIndex of itemTitles) & linefeed
end repeat
return outputText
