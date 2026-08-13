"""
CLI end-to-end tests for SAND.

Exercises `sand archive` and `sand restore` as real subprocess calls so every
line of the binary is exercised — compression, encryption, splitting, and
file I/O.

After the multi-file archive change, `sand archive` produces three zip files
(sand-p1.zip, sand-p2.zip, sand-p3.zip) in the output directory.  Each zip holds
the corresponding .pN.sand part for every input file.  Restore still accepts
individual .sand part files via --parts.
"""
import hashlib
import os
import subprocess
import zipfile

import pytest

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def run(sand_bin, *args):
    """Run the sand binary with the given arguments and return the result."""
    return subprocess.run(
        [sand_bin, *args],
        capture_output=True,
        text=True,
    )


def archive(sand_bin, src, password, out_dir):
    """
    Archive *src* and return the list of three extracted .sand part Paths.

    `sand archive` now produces sand-p1.zip / sand-p2.zip / sand-p3.zip.
    This helper extracts the individual .pN.sand files from those zips so
    that restore() can use them directly (the restore command still expects
    individual .sand part files, not zip files).
    """
    result = run(sand_bin, "archive", str(src), "--password", password, "--output-dir", str(out_dir))
    assert result.returncode == 0, f"archive failed:\n{result.stderr}"
    name = os.path.basename(str(src))
    parts = []
    for i in (1, 2, 3):
        zip_path = out_dir / f"sand-p{i}.zip"
        part_name = f"{name}.p{i}.sand"
        with zipfile.ZipFile(zip_path) as zf:
            zf.extract(part_name, out_dir)
        parts.append(out_dir / part_name)
    return parts


def archive_multi(sand_bin, srcs, password, out_dir):
    """
    Archive multiple source files and return the three zip Paths.
    """
    result = run(
        sand_bin, "archive",
        *[str(s) for s in srcs],
        "--password", password,
        "--output-dir", str(out_dir),
    )
    assert result.returncode == 0, f"archive failed:\n{result.stderr}"
    return [out_dir / f"sand-p{i}.zip" for i in (1, 2, 3)]


def restore(sand_bin, parts, password, out_dir):
    """Restore from the given part paths.  Returns the subprocess result."""
    return run(
        sand_bin,
        "restore",
        "--parts", ",".join(str(p) for p in parts),
        "--password", password,
        "--output-dir", str(out_dir),
    )


def extract_part(zip_path, out_dir, member=None):
    """Extract (optionally a specific member) from a zip and return extracted paths."""
    with zipfile.ZipFile(zip_path) as zf:
        members = [member] if member else zf.namelist()
        for m in members:
            zf.extract(m, out_dir)
        return [out_dir / m for m in members]


# ===========================================================================
# Archive — output format
# ===========================================================================

class TestArchive:
    def test_produces_three_zip_files(self, sand_bin, tmp_path):
        src = tmp_path / "hello.txt"
        src.write_text("Hello, SAND!")

        result = run(sand_bin, "archive", str(src), "--password", "pw", "--output-dir", str(tmp_path))

        assert result.returncode == 0
        for i in (1, 2, 3):
            assert (tmp_path / f"sand-p{i}.zip").exists(), f"sand-p{i}.zip not found"

    def test_zip_files_contain_correct_part_files(self, sand_bin, tmp_path):
        src = tmp_path / "hello.txt"
        src.write_text("Hello, SAND!")
        run(sand_bin, "archive", str(src), "--password", "pw", "--output-dir", str(tmp_path))

        for i in (1, 2, 3):
            with zipfile.ZipFile(tmp_path / f"sand-p{i}.zip") as zf:
                names = zf.namelist()
                assert len(names) == 1, f"sand-p{i}.zip should have 1 entry, got {names}"
                assert names[0].endswith(f".p{i}.sand"), f"entry should end in .p{i}.sand, got {names[0]}"

    def test_part_files_begin_with_SAND_magic(self, sand_bin, tmp_path):
        src = tmp_path / "magic.bin"
        src.write_bytes(b"\xDE\xAD\xBE\xEF" * 64)
        parts = archive(sand_bin, src, "pw", tmp_path)  # extracts .sand part files

        for i, path in enumerate(parts, 1):
            data = path.read_bytes()
            assert data[:4] == b"SAND", f"part {i} missing SAND magic"

    def test_all_three_parts_are_same_size(self, sand_bin, tmp_path):
        src = tmp_path / "equal.txt"
        src.write_bytes(b"X" * 1000)
        parts = archive(sand_bin, src, "pw", tmp_path)

        sizes = [p.stat().st_size for p in parts]
        assert sizes[0] == sizes[1] == sizes[2], f"part sizes differ: {sizes}"

    def test_nonexistent_input_fails(self, sand_bin, tmp_path):
        result = run(sand_bin, "archive", "/no/such/file.bin", "--password", "pw", "--output-dir", str(tmp_path))
        assert result.returncode != 0

    def test_output_dir_respected(self, sand_bin, tmp_path):
        src = tmp_path / "src.txt"
        src.write_text("data")
        out = tmp_path / "output"
        out.mkdir()

        run(sand_bin, "archive", str(src), "--password", "pw", "--output-dir", str(out))

        assert (out / "sand-p1.zip").exists()
        assert not (tmp_path / "sand-p1.zip").exists()

    def test_archive_design_pdf(self, sand_bin, design_pdf, tmp_path):
        result = run(sand_bin, "archive", design_pdf, "--password", "pdfpw", "--output-dir", str(tmp_path))
        assert result.returncode == 0, result.stderr
        for i in (1, 2, 3):
            assert (tmp_path / f"sand-p{i}.zip").exists()

    def test_success_message_printed(self, sand_bin, tmp_path):
        src = tmp_path / "msg.txt"
        src.write_text("check output")

        result = run(sand_bin, "archive", str(src), "--password", "pw", "--output-dir", str(tmp_path))

        assert "Archive complete" in result.stdout
        assert "sand-p1.zip" in result.stdout
        assert "Any 2 zips" in result.stdout


# ===========================================================================
# Archive — multiple files
# ===========================================================================

class TestArchiveMultipleFiles:
    def test_two_files_produce_three_zips(self, sand_bin, tmp_path):
        a = tmp_path / "alpha.txt"
        b = tmp_path / "beta.bin"
        a.write_text("file alpha")
        b.write_bytes(b"\x00\x01\x02\x03" * 100)

        result = run(
            sand_bin, "archive", str(a), str(b),
            "--password", "pw", "--output-dir", str(tmp_path),
        )
        assert result.returncode == 0, result.stderr

        for i in (1, 2, 3):
            assert (tmp_path / f"sand-p{i}.zip").exists()

    def test_each_zip_has_one_part_per_input_file(self, sand_bin, tmp_path):
        files = [tmp_path / f"f{i}.txt" for i in range(3)]
        for f in files:
            f.write_text(f"content of {f.name}")

        archive_multi(sand_bin, files, "pw", tmp_path)

        for i in (1, 2, 3):
            with zipfile.ZipFile(tmp_path / f"sand-p{i}.zip") as zf:
                names = zf.namelist()
                assert len(names) == 3, f"sand-p{i}.zip: expected 3 entries, got {names}"
                for name in names:
                    assert name.endswith(f".p{i}.sand"), f"entry {name!r} should end in .p{i}.sand"

    def test_each_file_restores_independently(self, sand_bin, tmp_path):
        orig_a = b"content of file A " + bytes(range(40))
        orig_b = b"content of file B " + bytes(range(50, 90))
        src_a = tmp_path / "fileA.bin"
        src_b = tmp_path / "fileB.bin"
        src_a.write_bytes(orig_a)
        src_b.write_bytes(orig_b)

        zip_paths = archive_multi(sand_bin, [src_a, src_b], "secret", tmp_path)

        # Extract part files for each source from the zips
        parts_a = []
        parts_b = []
        for i, zp in enumerate(zip_paths, 1):
            with zipfile.ZipFile(zp) as zf:
                for name in zf.namelist():
                    dest = tmp_path / "parts" / name
                    dest.parent.mkdir(exist_ok=True)
                    dest.write_bytes(zf.read(name))
                    if "fileA" in name:
                        parts_a.append(dest)
                    else:
                        parts_b.append(dest)

        # Restore file A using parts 1+2
        rd_a = tmp_path / "ra"
        rd_a.mkdir()
        r = restore(sand_bin, parts_a[:2], "secret", rd_a)
        assert r.returncode == 0, r.stderr
        assert (rd_a / "fileA.bin").read_bytes() == orig_a

        # Restore file B using parts 2+3
        rd_b = tmp_path / "rb"
        rd_b.mkdir()
        r = restore(sand_bin, parts_b[1:], "secret", rd_b)
        assert r.returncode == 0, r.stderr
        assert (rd_b / "fileB.bin").read_bytes() == orig_b

    def test_multi_file_message_mentions_file_count(self, sand_bin, tmp_path):
        files = [tmp_path / f"x{i}.txt" for i in range(2)]
        for f in files:
            f.write_text(f.name)

        result = run(
            sand_bin, "archive", *[str(f) for f in files],
            "--password", "pw", "--output-dir", str(tmp_path),
        )
        assert "2 files" in result.stdout

    def test_cross_file_parts_cannot_restore(self, sand_bin, tmp_path):
        """Parts from different files must not restore each other."""
        src_a = tmp_path / "one.txt"
        src_b = tmp_path / "two.txt"
        src_a.write_text("one one one")
        src_b.write_text("two two two")

        zip_paths = archive_multi(sand_bin, [src_a, src_b], "pw", tmp_path)

        # Extract: part1 from file "one", part2 from file "two"
        with zipfile.ZipFile(zip_paths[0]) as zf:
            for name in zf.namelist():
                if "one" in name:
                    dest1 = tmp_path / name
                    dest1.write_bytes(zf.read(name))

        with zipfile.ZipFile(zip_paths[1]) as zf:
            for name in zf.namelist():
                if "two" in name:
                    dest2 = tmp_path / name
                    dest2.write_bytes(zf.read(name))

        rd = tmp_path / "rd"
        rd.mkdir()
        result = restore(sand_bin, [dest1, dest2], "pw", rd)
        assert result.returncode != 0, "cross-file part mix should fail"


# ===========================================================================
# Restore — 2-of-3 combinations
# ===========================================================================

class TestRestoreCombinations:
    @pytest.mark.parametrize("combo", [(0, 1), (0, 2), (1, 2)], ids=["1+2", "1+3", "2+3"])
    def test_restore_two_of_three_parts(self, sand_bin, tmp_path, combo):
        original = b"Testing 2-of-3 restore: " + bytes(combo)
        src = tmp_path / "doc.bin"
        src.write_bytes(original)
        parts = archive(sand_bin, src, "pw", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        result = restore(sand_bin, [parts[combo[0]], parts[combo[1]]], "pw", rd)

        assert result.returncode == 0, result.stderr
        assert (rd / "doc.bin").read_bytes() == original

    def test_restore_all_three_parts(self, sand_bin, tmp_path):
        original = b"restore with all three"
        src = tmp_path / "all3.bin"
        src.write_bytes(original)
        parts = archive(sand_bin, src, "pw", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        result = restore(sand_bin, parts, "pw", rd)

        assert result.returncode == 0
        assert (rd / "all3.bin").read_bytes() == original

    def test_design_pdf_roundtrip_parts_1_and_3(self, sand_bin, design_pdf, tmp_path):
        original = open(design_pdf, "rb").read()
        orig_hash = hashlib.sha256(original).hexdigest()

        parts = archive(sand_bin, design_pdf, "pdfpassword", tmp_path)
        rd = tmp_path / "restored"
        rd.mkdir()

        result = restore(sand_bin, [parts[0], parts[2]], "pdfpassword", rd)

        assert result.returncode == 0, result.stderr
        restored_hash = hashlib.sha256((rd / "design.pdf").read_bytes()).hexdigest()
        assert orig_hash == restored_hash, "PDF content changed after round-trip"


# ===========================================================================
# Restore — security checks
# ===========================================================================

class TestRestoreSecurity:
    def test_wrong_password_fails(self, sand_bin, tmp_path):
        src = tmp_path / "secret.txt"
        src.write_text("top secret")
        parts = archive(sand_bin, src, "correct", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        result = restore(sand_bin, parts[:2], "wrong", rd)

        assert result.returncode != 0
        assert not (rd / "secret.txt").exists()

    def test_mismatched_archives_fail(self, sand_bin, tmp_path):
        src_a = tmp_path / "a.txt"
        src_b = tmp_path / "b.txt"
        src_a.write_text("archive A")
        src_b.write_text("archive B")

        out_a = tmp_path / "out_a"
        out_b = tmp_path / "out_b"
        out_a.mkdir()
        out_b.mkdir()

        parts_a = archive(sand_bin, src_a, "pw", out_a)
        parts_b = archive(sand_bin, src_b, "pw", out_b)

        rd = tmp_path / "restored"
        rd.mkdir()
        result = restore(sand_bin, [parts_a[0], parts_b[1]], "pw", rd)

        assert result.returncode != 0

    def test_corrupted_part_fails(self, sand_bin, tmp_path):
        src = tmp_path / "data.txt"
        src.write_text("important data")
        parts = archive(sand_bin, src, "pw", tmp_path)

        # Flip the last byte of part 1 (corrupts the GCM auth tag)
        raw = parts[0].read_bytes()
        parts[0].write_bytes(raw[:-1] + bytes([raw[-1] ^ 0xFF]))

        rd = tmp_path / "restored"
        rd.mkdir()
        result = restore(sand_bin, parts[:2], "pw", rd)

        assert result.returncode != 0

    def test_duplicate_part_fails(self, sand_bin, tmp_path):
        src = tmp_path / "dup.txt"
        src.write_text("duplicate part test")
        parts = archive(sand_bin, src, "pw", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        # Provide part 1 twice — same part number
        result = restore(sand_bin, [parts[0], parts[0]], "pw", rd)

        assert result.returncode != 0


# ===========================================================================
# Edge cases
# ===========================================================================

class TestEdgeCases:
    def test_empty_file_roundtrip(self, sand_bin, tmp_path):
        src = tmp_path / "empty.bin"
        src.write_bytes(b"")
        parts = archive(sand_bin, src, "pw", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        result = restore(sand_bin, parts[:2], "pw", rd)

        assert result.returncode == 0
        assert (rd / "empty.bin").read_bytes() == b""

    def test_single_byte_all_combinations(self, sand_bin, tmp_path):
        src = tmp_path / "one.bin"
        src.write_bytes(b"\xFF")
        parts = archive(sand_bin, src, "pw", tmp_path)

        for i, combo in enumerate([(0, 1), (0, 2), (1, 2)]):
            rd = tmp_path / f"r{i}"
            rd.mkdir()
            result = restore(sand_bin, [parts[combo[0]], parts[combo[1]]], "pw", rd)
            assert result.returncode == 0, f"combo {combo} failed: {result.stderr}"
            assert (rd / "one.bin").read_bytes() == b"\xFF"

    def test_binary_data_all_bytes_preserved(self, sand_bin, tmp_path):
        original = bytes(range(256)) * 40  # all 256 byte values, 10 KB
        src = tmp_path / "all_bytes.bin"
        src.write_bytes(original)
        parts = archive(sand_bin, src, "pw", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        restore(sand_bin, [parts[0], parts[2]], "pw", rd)

        assert (rd / "all_bytes.bin").read_bytes() == original

    @pytest.mark.slow
    def test_large_random_file(self, sand_bin, tmp_path):
        original = os.urandom(1024 * 1024)  # 1 MB
        src = tmp_path / "large.bin"
        src.write_bytes(original)
        parts = archive(sand_bin, src, "longpassword", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        result = restore(sand_bin, [parts[1], parts[2]], "longpassword", rd)

        assert result.returncode == 0
        assert (rd / "large.bin").read_bytes() == original

    def test_password_with_special_characters(self, sand_bin, tmp_path):
        original = b"special password test"
        src = tmp_path / "spec.bin"
        src.write_bytes(original)
        pw = "p@$$w0rd!#%-+="
        parts = archive(sand_bin, src, pw, tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        result = restore(sand_bin, parts[:2], pw, rd)

        assert result.returncode == 0
        assert (rd / "spec.bin").read_bytes() == original

    def test_original_filename_preserved(self, sand_bin, tmp_path):
        src = tmp_path / "my_report_2024.pdf"
        src.write_bytes(b"%PDF-1.4 fake content")
        parts = archive(sand_bin, src, "pw", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        restore(sand_bin, parts[:2], "pw", rd)

        assert (rd / "my_report_2024.pdf").exists()

    def test_only_one_part_fails(self, sand_bin, tmp_path):
        src = tmp_path / "t.txt"
        src.write_text("data")
        parts = archive(sand_bin, src, "pw", tmp_path)

        rd = tmp_path / "restored"
        rd.mkdir()
        result = run(sand_bin, "restore", "--parts", str(parts[0]), "--password", "pw", "--output-dir", str(rd))

        assert result.returncode != 0

    def test_four_parts_argument_fails(self, sand_bin, tmp_path):
        rd = tmp_path / "restored"
        rd.mkdir()
        result = run(sand_bin, "restore", "--parts", "a,b,c,d", "--password", "pw", "--output-dir", str(rd))
        assert result.returncode != 0

    @pytest.mark.parametrize("size", [0, 1, 2, 3, 127, 128, 129, 255, 256, 1023, 1024, 1025])
    def test_various_file_sizes_roundtrip(self, sand_bin, tmp_path, size):
        original = bytes(i % 251 for i in range(size))
        src = tmp_path / f"size_{size}.bin"
        src.write_bytes(original)
        parts = archive(sand_bin, src, "pw", tmp_path)

        for combo in [(0, 1), (0, 2), (1, 2)]:
            rd = tmp_path / f"r_{size}_{combo[0]}{combo[1]}"
            rd.mkdir()
            result = restore(sand_bin, [parts[combo[0]], parts[combo[1]]], "pw", rd)
            assert result.returncode == 0, f"size={size} combo={combo}: {result.stderr}"
            assert (rd / f"size_{size}.bin").read_bytes() == original
