_repokit() {
    local IFS=$'\n'
    local cur
    if (( COMP_POINT >= ${#COMP_LINE} )); then
        cur="${COMP_WORDS[COMP_CWORD]}"
    else
        local prefix="${COMP_LINE:0:COMP_POINT}"
        cur="${prefix##*[[:space:]]}"
    fi
    COMPREPLY=($(GO_FLAGS_COMPLETION=1 "${COMP_WORDS[0]}" "${COMP_WORDS[@]:1:COMP_CWORD-1}" "$cur" 2>/dev/null))
}
complete -F _repokit repokit
