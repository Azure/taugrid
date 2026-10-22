# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Table-driven reward function tests.

Run inside the training container (has torch, trl, etc.):
  python -m pytest examples/cobol-cpt-grpo/06-grpo/test_reward.py -v

Run locally (no torch/trl needed):
  python examples/cobol-cpt-grpo/06-grpo/test_reward.py
"""

import json
import os
import subprocess
import sys
import tempfile
import types
import unittest
import unittest.mock as mock
from concurrent.futures import ThreadPoolExecutor


os.environ.setdefault("WORLD_SIZE", "2")
os.environ.setdefault("RANK", "0")
os.environ.setdefault("LOCAL_RANK", "0")
os.environ.setdefault("OUTPUT_DIR", "/tmp/test_reward_out")
os.environ.setdefault("TRAIN_FILE", "/dev/null")

for mod_name in [
    "torch",
    "torch.cuda",
    "torch.distributed",
    "datasets",
    "transformers",
    "trl",
    "safetensors",
    "safetensors.torch",
    "safetensors._safetensors_rust",
    "deepspeed",
]:
    if mod_name not in sys.modules:
        fake = types.ModuleType(mod_name)
        if mod_name == "torch":
            fake.cuda = types.ModuleType("torch.cuda")
            fake.cuda.device_count = lambda: 0
            fake.bfloat16 = "bfloat16"
        if mod_name == "transformers":
            fake.AutoTokenizer = type("AutoTokenizer", (), {})
            fake.TrainerCallback = type("TrainerCallback", (), {})
        if mod_name == "trl":
            fake.GRPOConfig = type("GRPOConfig", (), {})
            fake.GRPOTrainer = type("GRPOTrainer", (), {})
        if mod_name == "datasets":
            fake.Dataset = type("Dataset", (), {})
        sys.modules[mod_name] = fake

sys.path.insert(0, os.path.dirname(__file__))

from train_grpo import (  # noqa: E402
    TauGRPOMetricsCallback,
    _compilation_reward,
    _count_cobol_verbs,
    _count_noop_verbs,
    _has_non_display_verb,
    _parse_cobc,
    _repeated_statement_ratio,
    _strip_non_code,
    _structural_reward,
    _try_compile,
    compile_format,
    load_prompts,
    reward_fn,
)


class TestStripNonCode(unittest.TestCase):
    def test_free_comment(self):
        text = "*> DISPLAY MOVE IF\n           DISPLAY X\n"
        stripped = _strip_non_code(text)
        self.assertNotIn("MOVE", stripped)
        self.assertIn("DISPLAY", stripped)

    def test_multiply_operator_at_column_7(self):
        text = " IF A * B > 0\n   MOVE A TO C\n   DISPLAY C\n END-IF.\n STOP RUN.\n"
        stripped = _strip_non_code(text)
        self.assertIn("IF", stripped)
        self.assertIn("MOVE", stripped)
        self.assertIn("DISPLAY", stripped)

    def test_divide_operator_at_column_7(self):
        text = " IF A / B > 0\n   MOVE A TO C\n   DISPLAY C\n END-IF.\n STOP RUN.\n"
        stripped = _strip_non_code(text)
        self.assertIn("IF", stripped)
        self.assertIn("MOVE", stripped)
        self.assertIn("DISPLAY", stripped)

    def test_six_space_D_not_stripped_in_free_format(self):
        text = "      DISPLAY X\n"
        stripped = _strip_non_code(text)
        self.assertIn("DISPLAY", stripped)

    def test_string_literal(self):
        text = """           DISPLAY "MOVE IF ADD"\n"""
        stripped = _strip_non_code(text)
        self.assertNotIn("MOVE", stripped)
        self.assertNotIn("ADD", stripped)
        self.assertIn("DISPLAY", stripped)

    def test_single_quoted_literal(self):
        text = "           DISPLAY 'MOVE IF'\n"
        stripped = _strip_non_code(text)
        self.assertNotIn("MOVE", stripped)
        self.assertIn("DISPLAY", stripped)

    def test_end_terminator(self):
        text = "           END-IF\n           END-PERFORM\n"
        stripped = _strip_non_code(text)
        self.assertNotIn("END-IF", stripped)
        self.assertNotIn("END-PERFORM", stripped)

    def test_hyphenated_identifier(self):
        text = "           MOVE-RECORD.\n           WS-COUNTER\n"
        stripped = _strip_non_code(text)
        self.assertNotIn("MOVE-RECORD", stripped)
        self.assertNotIn("MOVE", stripped.replace("_ID_", ""))


class TestCobolVerbCount(unittest.TestCase):
    def test_empty(self):
        self.assertEqual(_count_cobol_verbs(""), 0)

    def test_comments_containing_verbs_are_zero(self):
        text = "*> DISPLAY one\n*> MOVE two\n*> IF three\n"
        self.assertEqual(_count_cobol_verbs(text), 0)

    def test_string_containing_verbs(self):
        text = '           DISPLAY "MOVE IF"\n'
        self.assertEqual(_count_cobol_verbs(text), 1)

    def test_hyphenated_identifier_not_counted(self):
        text = "           MOVE-RECORD.\n"
        self.assertEqual(_count_cobol_verbs(text), 0)

    def test_end_if_not_counted(self):
        text = "           END-IF\n           END-PERFORM\n"
        self.assertEqual(_count_cobol_verbs(text), 0)

    def test_display_is_meaningful(self):
        self.assertEqual(_count_cobol_verbs('           DISPLAY "HELLO"\n'), 1)

    def test_stop_not_meaningful(self):
        self.assertEqual(_count_cobol_verbs("           STOP RUN.\n"), 0)

    def test_goback_not_meaningful(self):
        self.assertEqual(_count_cobol_verbs("           GOBACK.\n"), 0)

    def test_exit_not_meaningful(self):
        self.assertEqual(_count_cobol_verbs("           EXIT.\n"), 0)

    def test_continue_not_meaningful(self):
        self.assertEqual(_count_cobol_verbs("           CONTINUE.\n"), 0)

    def test_go_to_is_meaningful(self):
        self.assertEqual(_count_cobol_verbs("           GO TO PARA-1\n"), 1)

    def test_free_is_meaningful(self):
        self.assertEqual(_count_cobol_verbs("           FREE WS-PTR\n"), 1)

    def test_six_space_display_counted(self):
        text = "      DISPLAY X\n      DISPLAY Y\n      DISPLAY Z\n"
        self.assertEqual(_count_cobol_verbs(text), 3)

    def test_perform_move_if_display(self):
        text = (
            "           PERFORM INIT-PARA\n"
            "           MOVE 1 TO WS-X\n"
            "           IF WS-X > 0\n"
            "               DISPLAY WS-X\n"
            "           END-IF\n"
            "           STOP RUN.\n"
        )
        self.assertEqual(_count_cobol_verbs(text), 4)

    def test_case_insensitive(self):
        self.assertEqual(_count_cobol_verbs("display move perform"), 3)

    def test_multiply_at_column_7_not_stripped(self):
        text = " IF A * B > 0\n   MOVE A TO C\n   DISPLAY C\n END-IF.\n STOP RUN.\n"
        self.assertEqual(_count_cobol_verbs(text), 3)

    def test_divide_at_column_7_not_stripped(self):
        text = " IF A / B > 0\n   MOVE A TO C\n   DISPLAY C\n END-IF.\n STOP RUN.\n"
        self.assertEqual(_count_cobol_verbs(text), 3)


class TestNoopVerbCount(unittest.TestCase):
    def test_continue_is_noop(self):
        self.assertEqual(_count_noop_verbs("           CONTINUE.\n"), 1)

    def test_display_not_noop(self):
        self.assertEqual(_count_noop_verbs("           DISPLAY X.\n"), 0)

    def test_stop_not_noop(self):
        self.assertEqual(_count_noop_verbs("           STOP RUN.\n"), 0)

    def test_goback_not_noop(self):
        self.assertEqual(_count_noop_verbs("           GOBACK.\n"), 0)

    def test_exit_not_noop(self):
        self.assertEqual(_count_noop_verbs("           EXIT.\n"), 0)

    def test_go_not_noop(self):
        self.assertEqual(_count_noop_verbs("           GO TO PARA-1.\n"), 0)


class TestHasNonDisplayVerb(unittest.TestCase):
    def test_display_only(self):
        text = (
            '           DISPLAY "A"\n           DISPLAY "B"\n           DISPLAY "C"\n'
        )
        self.assertFalse(_has_non_display_verb(text))

    def test_move_present(self):
        text = "           MOVE 1 TO X\n           DISPLAY X\n"
        self.assertTrue(_has_non_display_verb(text))

    def test_go_to_present(self):
        text = "           GO TO PARA-1\n           DISPLAY X\n"
        self.assertTrue(_has_non_display_verb(text))

    def test_empty(self):
        self.assertFalse(_has_non_display_verb(""))

    def test_verb_in_literal_with_comment_marker(self):
        text = (
            '           DISPLAY "MOVE *> A".\n'
            '           DISPLAY "MOVE *> B".\n'
            '           DISPLAY "MOVE *> C".\n'
        )
        self.assertFalse(_has_non_display_verb(text))


class TestVerbCountWithCommentInLiteral(unittest.TestCase):
    def test_move_inside_literal_not_counted(self):
        text = (
            '           DISPLAY "MOVE *> A".\n'
            '           DISPLAY "MOVE *> B".\n'
            '           DISPLAY "MOVE *> C".\n'
            "           STOP RUN.\n"
        )
        self.assertEqual(_count_cobol_verbs(text), 3)

    def test_structural_penalty_for_display_only_with_literal_verbs(self):
        completion = (
            '           DISPLAY "MOVE *> A".\n'
            '           DISPLAY "MOVE *> B".\n'
            '           DISPLAY "MOVE *> C".\n'
            "           STOP RUN.\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.20)


class TestRepeatedStatementRatio(unittest.TestCase):
    def test_all_same(self):
        text = '           DISPLAY "A".\n' * 5
        ratio = _repeated_statement_ratio(text)
        self.assertGreater(ratio, 0.5)

    def test_all_different(self):
        text = (
            "           MOVE 1 TO X.\n"
            "           ADD 1 TO Y.\n"
            "           DISPLAY X.\n"
            "           DISPLAY Y.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertLessEqual(ratio, 0.5)

    def test_short_text(self):
        self.assertEqual(_repeated_statement_ratio("           DISPLAY X.\n"), 0.0)

    def test_distinct_moves_with_different_identifiers(self):
        text = (
            "           MOVE WS-A TO WS-B.\n"
            "           MOVE WS-C TO WS-D.\n"
            "           MOVE WS-E TO WS-F.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertLessEqual(ratio, 0.5)

    def test_same_statement_different_case_and_spacing(self):
        text = (
            "           MOVE 1 TO WS-X.\n"
            "           move  1 to ws-x.\n"
            "           MOVE\t1 TO WS-X.\n"
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertGreater(ratio, 0.5)

    def test_distinct_call_literals_not_repeated(self):
        text = (
            '           CALL "PROG-A" USING WS-X.\n'
            '           CALL "PROG-B" USING WS-X.\n'
            '           CALL "PROG-C" USING WS-X.\n'
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertLessEqual(ratio, 0.5)

    def test_comment_marker_inside_literal_preserved(self):
        text = (
            '           DISPLAY "PHASE *> A".\n'
            '           DISPLAY "PHASE *> B".\n'
            '           DISPLAY "PHASE *> C".\n'
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertLessEqual(ratio, 0.5)

    def test_same_statement_split_across_lines(self):
        text = (
            "           MOVE 1\n"
            "               TO WS-X.\n"
            "           MOVE 1\n"
            "               TO WS-X.\n"
            "           MOVE 1\n"
            "               TO WS-X.\n"
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertGreater(ratio, 0.5)

    def test_same_statement_different_line_breaks(self):
        text = (
            "           MOVE 1 TO WS-X.\n"
            "           MOVE 1\n"
            "               TO WS-X.\n"
            "           MOVE\n"
            "               1 TO\n"
            "               WS-X.\n"
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertGreater(ratio, 0.5)

    def test_intra_sentence_repetition(self):
        text = (
            "           MOVE 1 TO WS-Y\n"
            "           MOVE 1 TO WS-Y\n"
            "           MOVE 1 TO WS-Y\n"
            "           DISPLAY WS-Y.\n"
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertGreater(ratio, 0.5)

    def test_decimal_literal_not_split(self):
        text = (
            "           MOVE 1.1 TO WS-X.\n"
            "           MOVE 1.1 TO WS-X.\n"
            "           MOVE 1.1 TO WS-X.\n"
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertGreater(ratio, 0.5)

    def test_attached_comment_distinct_code(self):
        text = (
            '           DISPLAY "A".*> inline note\n'
            '           DISPLAY "B".*> inline note\n'
            '           DISPLAY "C".*> inline note\n'
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertLessEqual(ratio, 0.5)

    def test_attached_comment_same_code(self):
        text = (
            "           MOVE 1 TO WS-Y*> first\n"
            "           MOVE 1 TO WS-Y*> second\n"
            "           MOVE 1 TO WS-Y*> third\n"
            "           DISPLAY WS-Y.\n"
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertGreater(ratio, 0.5)

    def test_spaced_comment_same_code(self):
        text = (
            "           MOVE 1 TO WS-Y *> comment A\n"
            "           MOVE 1 TO WS-Y *> comment B\n"
            "           MOVE 1 TO WS-Y *> comment C\n"
            "           DISPLAY WS-Y.\n"
            "           STOP RUN.\n"
        )
        ratio = _repeated_statement_ratio(text)
        self.assertGreater(ratio, 0.5)


class TestCompilationReward(unittest.TestCase):
    def test_clean_compile(self):
        self.assertEqual(_compilation_reward(0, ""), 1.0)

    def test_warnings_only(self):
        stderr = "prog.cbl:10: warning: some warning"
        self.assertEqual(_compilation_reward(0, stderr), 0.7)

    def test_timeout_raises(self):
        with self.assertRaises(RuntimeError):
            _compilation_reward(-1, "compilation timeout")

    def test_cobc_not_found_raises(self):
        with self.assertRaises(RuntimeError):
            _compilation_reward(-1, "cobc not found")

    def test_unknown_infra_failure_raises(self):
        with self.assertRaises(RuntimeError):
            _compilation_reward(-1, "something unexpected")

    def test_cobc_signal_crash_scores_as_failed_compile(self):
        stderr = (
            "x.cbl:15: error: 'LINE-NUMBER' is not defined\n"
            "x.cbl:17: error: cannot specify both COLUMN and AT screen-location\n"
            "x.cbl:23: error: syntax error, unexpected DEPENDING\n"
            "free(): invalid size\n"
        )
        crashed = subprocess.CompletedProcess(["cobc"], -6, b"", stderr.encode())
        with mock.patch("train_grpo.subprocess.run", return_value=crashed):
            rc, err = _try_compile("IDENTIFICATION DIVISION.")
        self.assertEqual(rc, 1)
        self.assertAlmostEqual(_compilation_reward(rc, err), -0.6)

    def test_copybook_only(self):
        stderr = "prog.cbl:5: error: COPY-FILE.cpy: No such file or directory"
        self.assertEqual(_compilation_reward(1, stderr), 0.0)

    def test_one_error(self):
        stderr = "prog.cbl:10: error: 'WS-FOO' is not defined"
        self.assertAlmostEqual(_compilation_reward(1, stderr), -0.2)

    def test_two_errors(self):
        stderr = (
            "prog.cbl:10: error: 'WS-FOO' is not defined\n"
            "prog.cbl:11: error: 'WS-BAR' is not defined"
        )
        self.assertAlmostEqual(_compilation_reward(1, stderr), -0.4)

    def test_five_errors_clamped(self):
        lines = [f"prog.cbl:{i}: error: err{i}" for i in range(5)]
        self.assertAlmostEqual(_compilation_reward(1, "\n".join(lines)), -1.0)

    def test_six_errors_clamped(self):
        lines = [f"prog.cbl:{i}: error: err{i}" for i in range(6)]
        self.assertAlmostEqual(_compilation_reward(1, "\n".join(lines)), -1.0)

    def test_no_errors_no_warnings_nonzero_rc(self):
        self.assertEqual(_compilation_reward(1, ""), -1.0)

    def test_mixed_copybook_and_errors(self):
        stderr = (
            "prog.cbl:5: error: COPY-FILE.cpy: No such file or directory\n"
            "prog.cbl:10: error: 'WS-FOO' is not defined"
        )
        self.assertAlmostEqual(_compilation_reward(1, stderr), -0.2)


class TestStructuralReward(unittest.TestCase):
    def test_comment_only_hard_reject(self):
        completion = "*> filler\n" * 10
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.50)

    def test_two_meaningful_verb_program(self):
        completion = '           DISPLAY "HELLO"\n           MOVE 1 TO X.\n'
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.50)

    def test_three_displays_only_penalized(self):
        completion = (
            '           DISPLAY "A"\n           DISPLAY "B"\n           DISPLAY "C"\n'
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.20)

    def test_three_identical_displays_penalized(self):
        completion = '           DISPLAY "A"\n' * 3
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.20)

    def test_three_continues_degenerate(self):
        completion = "           CONTINUE.\n" * 3
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.50)

    def test_noop_majority_penalty(self):
        completion = (
            "           MOVE 1 TO X\n"
            "           DISPLAY Y\n"
            "           ADD 1 TO Z\n"
            "           CONTINUE.\n"
            "           CONTINUE.\n"
            "           CONTINUE.\n"
            "           CONTINUE.\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.30)

    def test_truncation_penalty(self):
        completion = (
            "           MOVE 1 TO X\n"
            "           ADD 1 TO Y\n"
            "           DISPLAY X\n"
            "           DISPLAY Y\n"
        )
        r_normal = _structural_reward(completion, 1024, 50)
        r_truncated = _structural_reward(completion, 1024, 1020)
        self.assertLess(r_truncated, r_normal)
        self.assertEqual(r_truncated, r_normal - 0.15)

    def test_perform_bonus_defined(self):
        completion = (
            "       INIT-PARA.\n"
            "           MOVE 1 TO WS-X.\n"
            "           PERFORM INIT-PARA\n"
            "           DISPLAY WS-X\n"
            "           ADD 1 TO WS-Y\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, 0.15)

    def test_dangling_perform_no_bonus(self):
        completion = (
            "           PERFORM NONEXISTENT-PARA\n"
            "           DISPLAY WS-X\n"
            "           MOVE 1 TO WS-Y\n"
            "           ADD 1 TO WS-Z\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, 0.0)

    def test_perform_until_not_paragraph(self):
        completion = (
            "           PERFORM UNTIL WS-EOF\n"
            "               READ INPUT-FILE\n"
            "               DISPLAY WS-REC\n"
            "           END-PERFORM\n"
            "           CLOSE INPUT-FILE\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, 0.0)

    def test_max_gen_zero_no_truncation_penalty(self):
        completion = (
            "           MOVE 1 TO X\n"
            "           ADD 1 TO Y\n"
            "           DISPLAY X\n"
            "           DISPLAY Y\n"
        )
        r = _structural_reward(completion, 0, 999)
        self.assertGreaterEqual(r, 0.0)

    def test_comments_with_verbs_rejected(self):
        completion = (
            "*> DISPLAY something\n"
            "*> MOVE something\n"
            "*> IF something\n"
            "*> PERFORM something\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.50)

    def test_useful_short_program_not_penalized(self):
        completion = (
            "           MOVE 1 TO WS-CTR\n"
            "           DISPLAY WS-CTR\n"
            "           ADD 1 TO WS-CTR\n"
            "           DISPLAY WS-CTR\n"
            "           STOP RUN.\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertGreaterEqual(r, 0.0)

    def test_distinct_moves_not_penalized(self):
        completion = (
            "           MOVE WS-A TO WS-B.\n"
            "           MOVE WS-C TO WS-D.\n"
            "           MOVE WS-E TO WS-F.\n"
            "           DISPLAY WS-B.\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertGreaterEqual(r, 0.0)

    def test_go_to_program_not_penalized(self):
        completion = (
            "           GO TO PARA-1\n"
            "           DISPLAY X\n"
            "           MOVE 1 TO Y\n"
            "           STOP RUN.\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertGreaterEqual(r, 0.0)

    def test_six_space_displays_not_stripped(self):
        completion = (
            "      DISPLAY X\n      MOVE 1 TO Y\n      ADD 1 TO Z\n      DISPLAY Z\n"
        )
        r = _structural_reward(completion, 1024, 50)
        self.assertGreaterEqual(r, 0.0)

    def test_repeated_statements_penalized(self):
        completion = "           MOVE 1 TO X.\n" * 5 + "           DISPLAY X.\n"
        r = _structural_reward(completion, 1024, 50)
        self.assertEqual(r, -0.20)


class TestParseCobc(unittest.TestCase):
    def test_empty(self):
        errors, warnings, copybook = _parse_cobc("")
        self.assertEqual(errors, [])
        self.assertEqual(warnings, [])
        self.assertEqual(copybook, [])

    def test_error(self):
        stderr = "prog.cbl:10: error: 'WS-X' is not defined"
        errors, warnings, copybook = _parse_cobc(stderr)
        self.assertEqual(len(errors), 1)
        self.assertEqual(len(copybook), 0)

    def test_copybook(self):
        stderr = "prog.cbl:5: error: COPY-FILE.cpy: No such file or directory"
        errors, warnings, copybook = _parse_cobc(stderr)
        self.assertEqual(len(errors), 0)
        self.assertEqual(len(copybook), 1)

    def test_warning(self):
        stderr = "prog.cbl:10: warning: unreachable code"
        errors, warnings, copybook = _parse_cobc(stderr)
        self.assertEqual(len(warnings), 1)
        self.assertEqual(len(errors), 0)

    def test_mixed(self):
        stderr = (
            "prog.cbl:5: error: COPY-FILE.cpy: No such file or directory\n"
            "prog.cbl:10: error: 'WS-X' is not defined\n"
            "prog.cbl:15: warning: unreachable code"
        )
        errors, warnings, copybook = _parse_cobc(stderr)
        self.assertEqual(len(errors), 1)
        self.assertEqual(len(copybook), 1)
        self.assertEqual(len(warnings), 1)


class TestScoreIntegration(unittest.TestCase):
    def test_clean_compile_plus_good_structure(self):
        r_compile = _compilation_reward(0, "")
        completion = (
            "       INIT-PARA.\n"
            "           MOVE 1 TO WS-X.\n"
            "           PERFORM INIT-PARA\n"
            "           DISPLAY WS-X\n"
            "           ADD 1 TO WS-Y\n"
        )
        r_struct = _structural_reward(completion, 1024, 50)
        total = r_compile + r_struct
        self.assertEqual(total, 1.15)

    def test_one_error_trivial_completion(self):
        r_compile = _compilation_reward(1, "prog.cbl:10: error: 'X' is not defined")
        r_struct = _structural_reward("           DISPLAY 1\n", 1024, 10)
        total = r_compile + r_struct
        self.assertEqual(total, -0.7)

    def test_comment_exploit_blocked(self):
        r_compile = _compilation_reward(0, "")
        completion = "*> DISPLAY MOVE IF ADD COMPUTE\n" * 5
        r_struct = _structural_reward(completion, 1024, 50)
        total = r_compile + r_struct
        self.assertEqual(total, 0.5)

    def test_three_continues_compile_but_degenerate(self):
        r_compile = _compilation_reward(0, "")
        completion = "           CONTINUE.\n" * 3
        r_struct = _structural_reward(completion, 1024, 50)
        total = r_compile + r_struct
        self.assertEqual(total, 0.5)

    def test_three_displays_not_max_reward(self):
        r_compile = _compilation_reward(0, "")
        completion = (
            '           DISPLAY "A"\n           DISPLAY "B"\n           DISPLAY "C"\n'
        )
        r_struct = _structural_reward(completion, 1024, 50)
        total = r_compile + r_struct
        self.assertEqual(total, 0.80)

    def test_useful_program_max_reward(self):
        r_compile = _compilation_reward(0, "")
        completion = (
            "           MOVE 1 TO WS-CTR\n"
            "           ADD 1 TO WS-CTR\n"
            "           DISPLAY WS-CTR\n"
            "           STOP RUN.\n"
        )
        r_struct = _structural_reward(completion, 1024, 50)
        total = r_compile + r_struct
        self.assertEqual(total, 1.0)

    def test_go_to_program_gets_full_reward(self):
        r_compile = _compilation_reward(0, "")
        completion = (
            "           GO TO PARA-1\n"
            "           DISPLAY X\n"
            "           MOVE 1 TO Y\n"
            "           STOP RUN.\n"
        )
        r_struct = _structural_reward(completion, 1024, 50)
        total = r_compile + r_struct
        self.assertEqual(total, 1.0)


FIXED_PROGRAM = (
    "       IDENTIFICATION DIVISION.\n"
    "       PROGRAM-ID. FIXEDP.\n"
    "       PROCEDURE DIVISION.\n" + "           DISPLAY 'X'.\n" * 6
)


def _ok(args, **kwargs):
    return subprocess.CompletedProcess(args, 0, b"", b"")


class TestCompileFormat(unittest.TestCase):
    def test_normalizes_stage_05_values(self):
        self.assertEqual(compile_format("free"), "free")
        self.assertEqual(compile_format("fixed"), "fixed")
        self.assertEqual(compile_format("fixed-copybook-only"), "fixed")
        self.assertEqual(compile_format("free-copybook-only"), "free")

    def test_missing_defaults_to_free(self):
        self.assertEqual(compile_format(None), "free")
        self.assertEqual(compile_format(""), "free")

    def test_unknown_raises(self):
        with self.assertRaises(ValueError):
            compile_format("failed")

    def test_try_compile_passes_format_flag(self):
        with mock.patch("train_grpo.subprocess.run", side_effect=_ok) as run:
            _try_compile("IDENTIFICATION DIVISION.", "fixed")
            _try_compile("IDENTIFICATION DIVISION.")
        self.assertEqual(run.call_args_list[0].args[0][2], "-fixed")
        self.assertEqual(run.call_args_list[1].args[0][2], "-free")


class TestLoadPromptsKeepsFormat(unittest.TestCase):
    def _write(self, records):
        f = tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False)
        with f:
            for rec in records:
                f.write(json.dumps(rec) + "\n")
        self.addCleanup(os.unlink, f.name)
        return f.name

    def test_preflight_compiles_each_record_in_its_format(self):
        path = self._write(
            [
                {"text": FIXED_PROGRAM, "compile_fmt": "fixed"},
                {
                    "text": FIXED_PROGRAM.replace("FIXEDP", "FREEP"),
                    "compile_fmt": "free",
                },
                {"text": FIXED_PROGRAM.replace("FIXEDP", "OLDREC")},
            ]
        )

        def fixed_only(args, **kwargs):
            rc = 0 if args[1] == "--version" or args[2] == "-fixed" else 1
            return subprocess.CompletedProcess(args, rc, b"", b"")

        with mock.patch("train_grpo.subprocess.run", side_effect=fixed_only):
            prompts = load_prompts(path)
        self.assertEqual([r["compile_fmt"] for r in prompts], ["fixed"])
        self.assertIn("FIXEDP", prompts[0]["prompt"])

    def test_without_preflight_returns_format(self):
        path = self._write(
            [{"text": FIXED_PROGRAM, "compile_fmt": "fixed-copybook-only"}]
        )
        with mock.patch("train_grpo.subprocess.run", side_effect=_ok):
            prompts = load_prompts(path, preflight=False)
        self.assertEqual(prompts[0]["compile_fmt"], "fixed")


class TestRewardFnUsesFormat(unittest.TestCase):
    def test_each_completion_compiled_in_its_prompt_format(self):
        seen = []

        def fake_compile(text, fmt="free"):
            seen.append((text, fmt))
            return 0, ""

        with (
            mock.patch("train_grpo.ProcessPoolExecutor", ThreadPoolExecutor),
            mock.patch("train_grpo._try_compile", side_effect=fake_compile),
        ):
            rewards = reward_fn(
                ["A", "B"],
                ["           MOVE 1 TO X\n", "           MOVE 2 TO Y\n"],
                compile_fmt=["fixed", "free"],
            )
        self.assertEqual(len(rewards), 2)
        self.assertEqual(
            sorted(seen),
            [
                ("A           MOVE 1 TO X\n", "fixed"),
                ("B           MOVE 2 TO Y\n", "free"),
            ],
        )

    def test_missing_format_column_defaults_to_free(self):
        seen = []

        def fake_compile(text, fmt="free"):
            seen.append(fmt)
            return 0, ""

        with (
            mock.patch("train_grpo.ProcessPoolExecutor", ThreadPoolExecutor),
            mock.patch("train_grpo._try_compile", side_effect=fake_compile),
        ):
            reward_fn(["A"], ["           MOVE 1 TO X\n"])
        self.assertEqual(seen, ["free"])


class TestAlarmStopIsShared(unittest.TestCase):
    def _step(self, cb, logs, agg, any_rank):
        control = types.SimpleNamespace(should_training_stop=False)
        state = types.SimpleNamespace(global_step=5)
        with (
            mock.patch("train_grpo._aggregate_reward_stats", return_value=agg),
            mock.patch("train_grpo._any_rank", side_effect=any_rank) as synced,
        ):
            cb.on_log(None, state, control, logs=logs)
        return control.should_training_stop, synced

    def test_non_zero_rank_without_publisher_stops_on_local_alarm(self):
        cb = TauGRPOMetricsCallback()
        cb.kl_alarm_count = 9
        stopped, synced = self._step(cb, {"kl": 6.0}, {}, lambda flag: flag)
        self.assertTrue(stopped)
        synced.assert_called_once_with(True)

    def test_stops_when_another_rank_raised_the_alarm(self):
        cb = TauGRPOMetricsCallback()
        stopped, synced = self._step(
            cb, {"kl": 0.1}, {"mean_verb_count": 10}, lambda flag: True
        )
        self.assertTrue(stopped)
        synced.assert_called_once_with(False)

    def test_no_alarm_keeps_training(self):
        cb = TauGRPOMetricsCallback()
        stopped, _ = self._step(
            cb, {"kl": 0.1}, {"mean_verb_count": 10}, lambda flag: flag
        )
        self.assertFalse(stopped)

    def test_only_publisher_rank_publishes(self):
        publisher = mock.Mock()
        self._step(
            TauGRPOMetricsCallback(publisher), {"kl": 0.1}, {}, lambda flag: flag
        )
        publisher.publish.assert_called_once()
        stopped, synced = self._step(
            TauGRPOMetricsCallback(), {"kl": 0.1}, {}, lambda flag: flag
        )
        synced.assert_called_once()

    def test_step_zero_skips_the_collective(self):
        cb = TauGRPOMetricsCallback()
        control = types.SimpleNamespace(should_training_stop=False)
        with mock.patch("train_grpo._any_rank") as synced:
            cb.on_log(
                None, types.SimpleNamespace(global_step=0), control, logs={"kl": 9}
            )
        synced.assert_not_called()


if __name__ == "__main__":
    unittest.main()
