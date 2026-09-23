# Proposal document formats (candidate, disabled by default)

The existing project's `[projects.file_work]` may set `document_validator` to
the absolute path of the CRM `ops/runtime/team_file_pdf.py` executable. Its parent
directory and file must pass the same host ownership/no-write checks as existing
file runtime configuration. Install the locked CRM Python environment at
`/opt/team-files`; the script's shebang uses that interpreter in isolated mode.
This setting enables PPTX, PNG/JPEG and PDF for the existing file host. An empty
setting retains the original DOCX/XLSX boundary. It creates no listener.

The PDF protocol is raw document bytes on stdin, at most 20 MiB; no arguments or
document paths. Only exit 0 and exactly `PDF_OK_V1\n` mean accepted. The gateway
applies a 15-second timeout; the helper bounds CPU, memory, pages and expanded
streams. No parser diagnostics are copied into user messages. A missing or failed
helper rejects the PDF before storage/delivery. Incoming, outgoing and quoted
cross-account artifacts use the same validator and existing immutable snapshots.

PPTX permits editable text, shapes, tables and embedded PNG/JPEG, plus bounded
printer metadata retained from normal presentation templates. Macros, embedded
Office objects/charts, media, external relationships and actions remain rejected.
Raster images are fully decoded with a 25-million-pixel ceiling. The existing
20-MiB file, four-input/40-MiB batch and 100-MiB expanded package limits remain.

Native Feishu images and image posts enter the same selected-file budget when
this setting is enabled. Locale variants must carry the same pictures. Unknown
replies never download resources. Word permits only PAGE, NUMPAGES, TOC and local
PAGEREF, with supported switches and balanced split/nested fields. No field is
silently removed. Legacy configuration keeps its original all-fields restriction.

The ordinary-user CRM renderer invokes `cc-connect-anc file-check <basename>`
with bounded Office bytes on stdin. This read-only command runs the same Office
validator before configuration, logging, sockets or receivers start; it prints
`OFFICE_OK_V1` on success. It performs no storage or delivery. The renderer then
updates Word fields/indexes, exports PDF and renders page previews. Real employee
and template acceptance remain separate from offline structure/render checks.
