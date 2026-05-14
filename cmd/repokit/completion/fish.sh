function __repokit_complete
    set -l tokens (commandline -opc)
    set -l current (commandline -ct)
    set -l args $tokens[2..-1] $current
    env GO_FLAGS_COMPLETION=1 $tokens[1] $args 2>/dev/null
end
complete -c repokit -f -a '(__repokit_complete)'
