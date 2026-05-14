#compdef repokit
_repokit() {
    local -a completions
    completions=("${(@f)$(GO_FLAGS_COMPLETION=1 "${words[1]}" "${(@)words[2,$CURRENT-1]}" "$PREFIX" 2>/dev/null)}")
    compadd -- "${completions[@]}"
}
compdef _repokit repokit
