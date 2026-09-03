function __sway_session_contexts --argument-names scope
    set -l output (command sway-session completion contexts $scope 2>/dev/null)
    or return 0

    for line in $output
        set -l fields (string split \t -- $line)
        test (count $fields) -eq 2
        or continue
        string match -rq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' -- $fields[1]
        or continue
        test "$fields[1]" != 00000000-0000-0000-0000-000000000000
        or continue
        test -n "$fields[2]"
        or continue
        printf '%s\t%s\n' $fields[1] $fields[2]
    end
end

function __sway_session_command
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l skip_next 0
    while test (count $tokens) -gt 0
        if test $skip_next -eq 1
            set skip_next 0
            set -e tokens[1]
            continue
        end
        switch $tokens[1]
            case --config
                set skip_next 1
                set -e tokens[1]
            case '--config=*'
                set -e tokens[1]
            case --json -h --help
                set -e tokens[1]
            case --
                return 0
            case '*'
                printf '%s\n' $tokens[1]
                return 0
        end
    end
    return 1
end

function __sway_session_no_command
    __sway_session_options_open
    or return 1
    set -l tokens (commandline -opc)
    test (count $tokens) -gt 0
    and test "$tokens[-1]" = --config
    and return 1
    not __sway_session_command >/dev/null
end

function __sway_session_options_open
    set -l tokens (commandline -opc)
    set -l skip_next 0
    set -l global_options_open 1
    for token in $tokens
        if test $skip_next -eq 1
            if test $global_options_open -eq 1
                contains -- "$token" --json -h --help
                and continue
            end
            test "$token" = --
            and set global_options_open 0
            set skip_next 0
            continue
        end
        if contains -- "$token" --json -h --help
            test $global_options_open -eq 1
            and continue
            return 1
        end
        switch $token
            case --config --socket --desktop-id --session --cwd --label --provider --id --workspace
                set skip_next 1
            case --
                set global_options_open 0
                return 1
        end
    end
    return 0
end

function __sway_session_global_options_open
    set -l tokens (commandline -opc)
    not contains -- -- $tokens
end

function __sway_session_marker_value_open
    __sway_session_options_open
    or return 1

    set -l command (__sway_session_command)
    test (count $command) -eq 1
    or return 1
    set -l app_command
    set -l value_options
    set -l bool_options
    switch $command[1]
        case terminal
            set value_options --project --context --cwd --label --socket --role
            set bool_options --new --ephemeral
        case register
            set value_options --session --cwd --label --provider --id
        case restore daemon broker
            set value_options --socket
            test "$command[1]" = restore
            and set bool_options --require-active
        case request-start
            set value_options --session --cwd --label --provider --workspace
        case app
            set app_command (__sway_session_app_subcommand)
            test (count $app_command) -eq 1
            or return 1
            switch $app_command[1]
                case register-focused rebind-focused
                    set value_options --socket --desktop-id
                    set bool_options --yes
                case register-workspace status forget
                    set value_options --socket
                    test "$app_command[1]" = status
                    or set bool_options --yes
                case '*'
                    return 1
            end
        case '*'
            return 1
    end

    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l command_seen 0
    set -l app_command_seen 0
    set -l skip_next 0
    set -l marker_value_seen 0
    set -l global_options_open 1
    for token in $tokens
        if test $command_seen -eq 0
            if test "$token" = "$command[1]"
                set command_seen 1
            end
            continue
        end
        if test "$command[1]" = app; and test $app_command_seen -eq 0
            if test "$token" = "$app_command[1]"
                set app_command_seen 1
            end
            continue
        end
        if test $skip_next -eq 1
            if test $global_options_open -eq 1
                contains -- "$token" --json -h --help
                and continue
            end
            if test "$token" = --
                set marker_value_seen 1
                set global_options_open 0
            end
            set skip_next 0
            continue
        end
        if contains -- "$token" $value_options
            set skip_next 1
            continue
        end
        if contains -- "$token" $bool_options
            continue
        end
        switch $token
            case --
                set global_options_open 0
                return 1
            case --json -h --help
                test $global_options_open -eq 0
                and return 1
                continue
            case --'*'
                return 1
            case '*'
                return 1
        end
    end
    test $marker_value_seen -eq 1
    and test $skip_next -eq 0
end

function __sway_session_command_options
    set -l command (__sway_session_command)
    switch $command[1]
        case register
            printf '%s\n' --session --cwd --label --provider --id
        case restore
            printf '%s\n' --socket --require-active
        case daemon broker
            printf '%s\n' --socket
        case request-start
            printf '%s\n' --session --cwd --label --provider --workspace
        case app
            set -l app_command (__sway_session_app_subcommand)
            switch $app_command[1]
                case register-focused rebind-focused
                    printf '%s\n' --socket --desktop-id --yes
                case register-workspace
                    printf '%s\n' --socket --yes
                case status
                    printf '%s\n' --socket
                case forget
                    printf '%s\n' --socket --yes
            end
    end
end

function __sway_session_terminal_subcommand --argument-names wanted
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l seen_terminal 0
    set -l skip_next 0
    for token in $tokens
        if test $skip_next -eq 1
            set skip_next 0
            continue
        end
        if test $seen_terminal -eq 0
            switch $token
                case --config
                    set skip_next 1
                case '--config=*'
                case --json -h --help
                case terminal
                    set seen_terminal 1
                case '*'
                    return 1
            end
        else
            switch $token
                case --json -h --help
                case $wanted
                    return 0
                case '*'
                    return 1
            end
        end
    end
    return 1
end

function __sway_session_terminal_status_context_pending
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l skip_next 0
    set -l state
    for token in $tokens
        if test $skip_next -eq 1
            set skip_next 0
            continue
        end
        switch $token
            case --config
                set skip_next 1
            case '--config=*' --json -h --help
            case terminal
                test -z "$state"
                or return 1
                set state terminal
            case status
                test "$state" = terminal
                or return 1
                set state status
            case '*'
                return 1
        end
    end
    test "$state" = status
end

function __sway_session_terminal_rename_context_pending
    __sway_session_terminal_subcommand rename
    or return 1
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l seen_rename 0
    set -l skip_next 0
    for token in $tokens
        if test $skip_next -eq 1
            set skip_next 0
            continue
        end
        if test $seen_rename -eq 0
            test "$token" = rename
            and set seen_rename 1
            continue
        end
        switch $token
            case --label
                set skip_next 1
            case --json -h --help
            case --'*'
            case '*'
                return 1
        end
    end
    test $seen_rename -eq 1
    and test $skip_next -eq 0
end

function __sway_session_is_command --argument-names wanted
    set -l actual (__sway_session_command)
    test (count $actual) -eq 1
    and test "$actual[1]" = "$wanted"
end

function __sway_session_completion_contexts_pending
    __sway_session_is_command completion
    or return 1
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l found_contexts 0
    for token in $tokens
        switch $token
            case --json -h --help completion
                continue
            case contexts
                test $found_contexts -eq 0
                or return 1
                set found_contexts 1
            case '*'
                return 1
        end
    end
    test $found_contexts -eq 1
end

function __sway_session_completion_subcommand_pending
    __sway_session_is_command completion
    or return 1
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l found_command 0
    for token in $tokens
        switch $token
            case --json -h --help
                continue
            case completion
                test $found_command -eq 0
                or return 1
                set found_command 1
            case '*'
                return 1
        end
    end
    test $found_command -eq 1
end

function __sway_session_app_subcommand
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l found_command 0
    while test (count $tokens) -gt 0
        set -l token $tokens[1]
        set -e tokens[1]
        if test $found_command -eq 0
            switch $token
                case --json -h --help
                    continue
                case app
                    set found_command 1
                case '*'
                    return 0
            end
        else
            switch $token
                case --json -h --help
                    continue
                case '*'
                    printf '%s\n' $token
                    return 0
            end
        end
    end
    return 1
end

function __sway_session_is_app_subcommand --argument-names wanted
    __sway_session_is_command app
    or return 1
    set -l actual (__sway_session_app_subcommand)
    test (count $actual) -eq 1
    and test "$actual[1]" = "$wanted"
end

function __sway_session_top_context_pending --argument-names wanted
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l command
    while test (count $tokens) -gt 0
        switch $tokens[1]
            case --json -h --help
                set -e tokens[1]
            case '*'
                set command $tokens[1]
                set -e tokens[1]
                break
        end
    end
    test "$command" = "$wanted"
    or return 1

    set -l skip_next 0
    set -l options_ended 0
    set -l global_options_open 1
    for token in $tokens
        if test $skip_next -eq 1
            if test $global_options_open -eq 1
                contains -- "$token" --json -h --help
                and continue
            end
            test "$token" = --
            and set global_options_open 0
            set skip_next 0
            continue
        end
        if test $options_ended -eq 1
            return 1
        end
        if contains -- "$token" --json -h --help
            test $global_options_open -eq 1
            and continue
            return 1
        end
        switch $token
            case --socket
                set skip_next 1
            case --
                set global_options_open 0
                contains -- "$wanted" archive activate
                and return 1
                set options_ended 1
            case --yes --require-active
            case --'*'
            case '*'
                return 1
        end
    end
    test $skip_next -eq 0
end

function __sway_session_restore_contexts
    set -l tokens (commandline -opc)
    set -l skip_next 0
    set -l global_options_open 1
    for token in $tokens
        if test $skip_next -eq 1
            if test $global_options_open -eq 1
                contains -- "$token" --json -h --help
                and continue
            end
            test "$token" = --
            and set global_options_open 0
            set skip_next 0
            continue
        end
        if contains -- "$token" --json -h --help
            test $global_options_open -eq 1
            and continue
            break
        end
        switch $token
            case --socket
                set skip_next 1
            case --
                set global_options_open 0
                break
            case --require-active
                __sway_session_contexts restore-active
                return
        end
    end
    __sway_session_contexts restore
end

function __sway_session_app_context_pending --argument-names wanted
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l command
    while test (count $tokens) -gt 0
        switch $tokens[1]
            case --json -h --help
                set -e tokens[1]
            case '*'
                set command $tokens[1]
                set -e tokens[1]
                break
        end
    end
    test "$command" = app
    or return 1
    test (count $tokens) -gt 0
    or return 1
    test "$tokens[1]" = "$wanted"
    or return 1
    set -e tokens[1]

    set -l skip_next 0
    set -l options_ended 0
    set -l global_options_open 1
    for token in $tokens
        if test $skip_next -eq 1
            if test $global_options_open -eq 1
                contains -- "$token" --json -h --help
                and continue
            end
            test "$token" = --
            and set global_options_open 0
            set skip_next 0
            continue
        end
        if test $options_ended -eq 1
            return 1
        end
        if contains -- "$token" --json -h --help
            test $global_options_open -eq 1
            and continue
            return 1
        end
        switch $token
            case --socket --desktop-id
                set skip_next 1
            case --
                set global_options_open 0
                set options_ended 1
            case --yes
            case --'*'
            case '*'
                return 1
        end
    end
    test $skip_next -eq 0
end

complete -c sway-session -f
complete -c sway-session -n '__sway_session_global_options_open' -l json -d 'Emit machine-readable results and diagnostics'
complete -c sway-session -n '__sway_session_global_options_open' -s h -d 'Show help'
complete -c sway-session -n '__sway_session_global_options_open' -l help -d 'Show help'
complete -c sway-session -n '__sway_session_global_options_open' -l config -r -F
complete -c sway-session -n '__sway_session_no_command' -a 'register restore list archive activate purge app daemon broker request-start report-codex-session completion terminal'
complete -c sway-session -n '__sway_session_marker_value_open' -a '(__sway_session_command_options)'

complete -c sway-session -n '__sway_session_is_command register; and __sway_session_options_open' -l session -x
complete -c sway-session -n '__sway_session_is_command register; and __sway_session_options_open' -l cwd -r -F
complete -c sway-session -n '__sway_session_is_command register; and __sway_session_options_open' -l label -x
complete -c sway-session -n '__sway_session_is_command register; and __sway_session_options_open' -l provider -x
complete -c sway-session -n '__sway_session_is_command register; and __sway_session_options_open' -l id -x

complete -c sway-session -n '__sway_session_top_context_pending restore; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_top_context_pending restore; and __sway_session_options_open' -l require-active
complete -c sway-session -n '__sway_session_top_context_pending archive' -a '(__sway_session_contexts archive)'
complete -c sway-session -n '__sway_session_top_context_pending activate' -a '(__sway_session_contexts activate)'
complete -c sway-session -n '__sway_session_top_context_pending restore' -a '(__sway_session_restore_contexts)'
complete -c sway-session -n '__sway_session_top_context_pending purge' -a '(__sway_session_contexts purge)'
complete -c sway-session -n '__sway_session_top_context_pending purge; and __sway_session_options_open' -l yes -d 'Confirm non-interactively'

complete -c sway-session -n '__sway_session_is_command daemon; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_is_command broker; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_is_command request-start; and __sway_session_options_open' -l session -x
complete -c sway-session -n '__sway_session_is_command request-start; and __sway_session_options_open' -l cwd -r -F
complete -c sway-session -n '__sway_session_is_command request-start; and __sway_session_options_open' -l label -x
complete -c sway-session -n '__sway_session_is_command request-start; and __sway_session_options_open' -l provider -x
complete -c sway-session -n '__sway_session_is_command request-start; and __sway_session_options_open' -l workspace -x

complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open; and not __sway_session_terminal_subcommand list; and not __sway_session_terminal_subcommand status; and not __sway_session_terminal_subcommand cleanup; and not __sway_session_terminal_subcommand manage; and not __sway_session_terminal_subcommand rename; and not __sway_session_terminal_subcommand reconfigure' -l project -x
complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open; and not __sway_session_terminal_subcommand list; and not __sway_session_terminal_subcommand status; and not __sway_session_terminal_subcommand cleanup; and not __sway_session_terminal_subcommand manage; and not __sway_session_terminal_subcommand rename; and not __sway_session_terminal_subcommand reconfigure' -l context -x -a '(__sway_session_contexts terminal-status)'
complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open; and not __sway_session_terminal_subcommand list; and not __sway_session_terminal_subcommand status; and not __sway_session_terminal_subcommand cleanup; and not __sway_session_terminal_subcommand manage; and not __sway_session_terminal_subcommand rename; and not __sway_session_terminal_subcommand reconfigure' -l cwd -r -F
complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open; and not __sway_session_terminal_subcommand list; and not __sway_session_terminal_subcommand status; and not __sway_session_terminal_subcommand cleanup; and not __sway_session_terminal_subcommand manage; and not __sway_session_terminal_subcommand rename; and not __sway_session_terminal_subcommand reconfigure' -l label -x
complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open; and not __sway_session_terminal_subcommand list; and not __sway_session_terminal_subcommand status; and not __sway_session_terminal_subcommand cleanup; and not __sway_session_terminal_subcommand manage; and not __sway_session_terminal_subcommand rename; and not __sway_session_terminal_subcommand reconfigure' -l socket -r -F
complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open; and not __sway_session_terminal_subcommand list; and not __sway_session_terminal_subcommand status; and not __sway_session_terminal_subcommand cleanup; and not __sway_session_terminal_subcommand manage; and not __sway_session_terminal_subcommand rename; and not __sway_session_terminal_subcommand reconfigure' -l role -x -a 'shell agy amp claude cline codex copilot cursor devin droid gemini grok hermes kilo kimi kiro maki mastracode omp opencode pi qodercli qwen'
complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open; and not __sway_session_terminal_subcommand list; and not __sway_session_terminal_subcommand status; and not __sway_session_terminal_subcommand cleanup; and not __sway_session_terminal_subcommand manage; and not __sway_session_terminal_subcommand rename; and not __sway_session_terminal_subcommand reconfigure' -l new
complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open; and not __sway_session_terminal_subcommand list; and not __sway_session_terminal_subcommand status; and not __sway_session_terminal_subcommand cleanup; and not __sway_session_terminal_subcommand manage; and not __sway_session_terminal_subcommand rename; and not __sway_session_terminal_subcommand reconfigure' -l ephemeral
complete -c sway-session -n '__sway_session_is_command terminal; and __sway_session_options_open' -a 'manage list status cleanup rename reconfigure'
complete -c sway-session -n '__sway_session_terminal_subcommand manage; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_terminal_subcommand rename; and __sway_session_options_open' -l label -x
complete -c sway-session -n '__sway_session_terminal_rename_context_pending' -a '(__sway_session_contexts terminal-status)'
complete -c sway-session -n '__sway_session_terminal_subcommand reconfigure; and __sway_session_options_open' -l project -x
complete -c sway-session -n '__sway_session_terminal_subcommand reconfigure; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_terminal_status_context_pending' -a '(__sway_session_contexts terminal-status)'
complete -c sway-session -n '__sway_session_terminal_subcommand status; and __sway_session_options_open' -l project -x
complete -c sway-session -n '__sway_session_terminal_subcommand cleanup; and __sway_session_options_open' -l archived-before -x

complete -c sway-session -n '__sway_session_completion_subcommand_pending; and __sway_session_options_open' -a contexts
complete -c sway-session -n '__sway_session_completion_contexts_pending; and __sway_session_options_open' -a 'archive activate restore restore-active purge terminal-status app-forget'

complete -c sway-session -n '__sway_session_is_command app; and __sway_session_options_open; and not __sway_session_app_subcommand >/dev/null' -a 'register-focused register-workspace confirm status list rebind-focused reapprove pin unpin archive activate forget'
complete -c sway-session -n '__sway_session_is_app_subcommand register-focused; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_is_app_subcommand register-focused; and __sway_session_options_open' -l desktop-id -x
complete -c sway-session -n '__sway_session_is_app_subcommand register-focused; and __sway_session_options_open' -l yes
complete -c sway-session -n '__sway_session_is_app_subcommand register-workspace; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_is_app_subcommand register-workspace; and __sway_session_options_open' -l yes
complete -c sway-session -n '__sway_session_is_app_subcommand status; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_app_context_pending rebind-focused; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_app_context_pending rebind-focused; and __sway_session_options_open' -l desktop-id -x
complete -c sway-session -n '__sway_session_app_context_pending rebind-focused; and __sway_session_options_open' -l yes
complete -c sway-session -n '__sway_session_app_context_pending reapprove; and __sway_session_options_open' -l yes
complete -c sway-session -n '__sway_session_app_context_pending forget; and __sway_session_options_open' -l socket -r -F
complete -c sway-session -n '__sway_session_app_context_pending forget; and __sway_session_options_open' -l yes
complete -c sway-session -n '__sway_session_app_context_pending forget' -a '(__sway_session_contexts app-forget)'
