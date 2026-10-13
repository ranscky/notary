//go:build windows

package sign

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Windows has no POSIX permission bits to compare against: os.FileMode.Perm
// reports 0666 for every file, whatever was requested, because os.Chmod there
// toggles only the read-only attribute and the real access control lives in the
// file's discretionary ACL. So "may anyone but the owner read this key file?"
// is answered from the ACL instead of from the mode.
//
// A private file on Windows grants access to exactly three identities: the
// file's owner, SYSTEM, and the Administrators group. That is the analogue of a
// 0600 file -- nobody else can read it -- so this file allows those three and
// refuses any other ACE that grants read access.
//
// Every failure below fails CLOSED. A key file whose ACL cannot be read is not
// a key file this package can certify as private, and guessing in the
// operator's favour is the one direction that leaks the key.

const (
	// seFileObject is SE_FILE_OBJECT, the SE_OBJECT_TYPE value for a file.
	seFileObject = 1
	// ownerSecurityInformation is OWNER_SECURITY_INFORMATION.
	ownerSecurityInformation = 0x00000001
	// daclSecurityInformation is DACL_SECURITY_INFORMATION, asking for the
	// object's discretionary ACL alongside its owner.
	daclSecurityInformation = 0x00000004
	// aclSizeInformation is AclSizeInformation, the ACL_INFORMATION_CLASS that
	// yields an ACL_SIZE_INFORMATION, and through it the ACE count.
	aclSizeInformation = 2
	// accessAllowedAceType is ACCESS_ALLOWED_ACE_TYPE.
	accessAllowedAceType = 0x00
	// inheritOnlyAce is INHERIT_ONLY_ACE: the ACE is inherited by the object's
	// children but does not itself apply to this object, so it grants nothing
	// here and must not be counted as a reader.
	inheritOnlyAce = 0x08
	// sidOffsetInAccessAllowedAce is the byte offset at which the SID begins
	// inside an ACCESS_ALLOWED_ACE: sizeof(ACE_HEADER) == 4 plus
	// sizeof(ACCESS_MASK) == 4.
	sidOffsetInAccessAllowedAce = 8
)

// readAccessMask is the set of access bits that let a holder read the key
// material, named individually plus the two generic rights that imply them.
// FILE_GENERIC_READ is deliberately absent: it is not a distinct bit but a
// combination of FILE_READ_DATA, FILE_READ_EA, FILE_READ_ATTRIBUTES and
// READ_CONTROL, every one of which is listed here, so testing those bits
// already covers it.
//
// The test is "any of these bits", so an ACE carrying an unrecognized
// combination is refused rather than allowed. Over-refusing sends the operator
// to the help text; under-refusing writes a key that someone else can read.
const readAccessMask = uint32(
	0x00000001 | // FILE_READ_DATA
		0x00000008 | // FILE_READ_EA
		0x00000080 | // FILE_READ_ATTRIBUTES
		0x00020000 | // READ_CONTROL
		0x80000000 | // GENERIC_READ
		0x10000000) // GENERIC_ALL

// Well-known SIDs that may hold read access to a private key file besides its
// owner. Both are strictly more privileged than the process reading the key:
// SYSTEM is the operating system, and Administrators can take ownership of any
// file whatever its ACL says -- so an ACL that excluded them would be a
// restriction in appearance only, and refusing a file that grants them access
// would refuse every file on a normally configured machine.
const (
	systemSID         = "S-1-5-18"
	administratorsSID = "S-1-5-32-544"
)

// acl is the opaque Win32 ACL. Only its address is ever used: the ACEs inside
// it are walked through GetAclInformation and GetAce, which is why no layout is
// reproduced here.
type acl struct{}

// aclSizeInformationStruct mirrors the Win32 ACL_SIZE_INFORMATION, the struct
// GetAclInformation fills in for AclSizeInformation.
type aclSizeInformationStruct struct {
	aceCount      uint32
	aclBytesInUse uint32
	aclBytesFree  uint32
}

// aceHeader mirrors the Win32 ACE_HEADER, the first field of every ACE.
type aceHeader struct {
	aceType  uint8
	aceFlags uint8
	aceSize  uint16
}

// accessAllowedAce mirrors ACCESS_ALLOWED_ACE up to its Mask field; the SID
// follows at sidOffsetInAccessAllowedAce, where the Win32 struct's SidStart
// (the first DWORD of the variable-length SID) begins.
type accessAllowedAce struct {
	header aceHeader
	mask   uint32
}

// advapi32 is resolved lazily and only for the three functions below. Going
// through NewLazyDLL rather than a security package is deliberate: it keeps
// this check inside the standard library, so key hygiene costs the project no
// dependency.
var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procGetNamedSecurityInfoW = advapi32.NewProc("GetNamedSecurityInfoW")
	procGetAclInformation     = advapi32.NewProc("GetAclInformation")
	procGetAce                = advapi32.NewProc("GetAce")
)

// checkKeyFileAccess reports whether the key file at path is private enough to
// hold key material, returning a wrapped ErrLoosePerms when it is not.
//
// info is unused here: the mode it carries reports 0666 for every file on
// Windows, so the ACL is the only source of truth. The parameter is kept so
// that both platforms share one signature, and the POSIX implementation, where
// the mode is exactly what matters, uses it.
func checkKeyFileAccess(path string, _ os.FileInfo) error {
	sd, owner, dacl, err := securityDescriptor(path)
	if err != nil {
		return fmt.Errorf("sign: cannot read the ACL of %s: %w", path, err)
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(sd)) }()

	reader, err := firstForeignReader(dacl, owner)
	if err != nil {
		return fmt.Errorf("sign: cannot walk the ACL of %s: %w", path, err)
	}
	if reader != "" {
		return fmt.Errorf("%w: %s is readable by %s, which is neither its owner, SYSTEM, nor Administrators",
			ErrLoosePerms, path, reader)
	}
	return nil
}

// securityDescriptor fetches the security descriptor of path and returns its
// address (which the caller must LocalFree), its owner SID, and its DACL.
func securityDescriptor(path string) (sd uintptr, owner *syscall.SID, dacl *acl, err error) {
	objectName, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, nil, nil, err
	}

	var (
		ownerPtr *syscall.SID
		daclPtr  *acl
		sdPtr    uintptr
	)
	// GetNamedSecurityInfoW takes the ppSidGroup and ppSacl outputs positionally
	// and permits them to be nil, which is what is passed here: neither has any
	// bearing on who may read the file.
	ret, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(objectName)),
		seFileObject,
		ownerSecurityInformation|daclSecurityInformation,
		uintptr(unsafe.Pointer(&ownerPtr)),
		0,
		uintptr(unsafe.Pointer(&daclPtr)),
		0,
		uintptr(unsafe.Pointer(&sdPtr)),
	)
	if ret != 0 { // ERROR_SUCCESS is 0
		return 0, nil, nil, syscall.Errno(ret)
	}
	return sdPtr, ownerPtr, daclPtr, nil
}

// firstForeignReader returns the SID, in its string form, of the first identity
// in dacl that the file grants read access to and that is neither the file's
// owner, SYSTEM, nor Administrators. An empty return means no such identity
// exists, which is the answer that lets the key load.
//
// It returns an error rather than a verdict when the ACL cannot be walked, so
// that an unreadable ACL is refused rather than assumed benign.
func firstForeignReader(dacl *acl, owner *syscall.SID) (string, error) {
	if dacl == nil {
		// A null DACL grants full access to everyone; it is the precise
		// opposite of a private file, so it has to be refused rather than
		// treated as "no restrictions to check".
		return "everyone (the file carries no ACL)", nil
	}

	var size aclSizeInformationStruct
	ret, _, _ := procGetAclInformation.Call(
		uintptr(unsafe.Pointer(dacl)),
		uintptr(unsafe.Pointer(&size)),
		unsafe.Sizeof(size),
		aclSizeInformation,
	)
	if ret == 0 {
		return "", fmt.Errorf("GetAclInformation failed (%w)", syscall.GetLastError())
	}

	ownerSID, err := sidString(owner)
	if err != nil {
		return "", fmt.Errorf("reading the file's owner: %w", err)
	}

	for i := uint32(0); i < size.aceCount; i++ {
		var pAce unsafe.Pointer
		ret, _, _ := procGetAce.Call(
			uintptr(unsafe.Pointer(dacl)),
			uintptr(i),
			uintptr(unsafe.Pointer(&pAce)),
		)
		if ret == 0 {
			return "", fmt.Errorf("GetAce failed for ACE %d (%w)", i, syscall.GetLastError())
		}
		if pAce == nil {
			return "", fmt.Errorf("GetAce returned no ACE for index %d", i)
		}

		ace := (*accessAllowedAce)(pAce)
		if ace.header.aceType != accessAllowedAceType {
			// Only grants can widen access. A denied ACE cannot, and an audit
			// or object ACE is not an access grant at all.
			continue
		}
		if ace.header.aceFlags&inheritOnlyAce != 0 {
			// Inherited by children only: it says nothing about this file.
			continue
		}
		if ace.mask&readAccessMask == 0 {
			// Grants something, but not the ability to read the key.
			continue
		}

		sid, err := sidString((*syscall.SID)(unsafe.Add(pAce, sidOffsetInAccessAllowedAce)))
		if err != nil {
			// A grant this code cannot attribute cannot be cleared, so it is
			// refused: the alternative is allowing read access to an unknown
			// identity, which is exactly the outcome the check exists to stop.
			return "", fmt.Errorf("ACE %d grants read access to an unattributable SID: %w", i, err)
		}
		if sid == ownerSID || sid == systemSID || sid == administratorsSID {
			continue
		}
		return sid, nil
	}
	return "", nil
}

// sidString renders a SID in its canonical S-1-5-... form, which is
// locale-independent -- unlike the account names the SIDs resolve to.
func sidString(sid *syscall.SID) (string, error) {
	if sid == nil {
		return "", fmt.Errorf("no SID")
	}
	s, err := sid.String()
	if err != nil {
		return "", err
	}
	return s, nil
}
